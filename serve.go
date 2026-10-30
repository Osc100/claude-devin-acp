package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Devin as a model (`--serve`): an OpenAI-compatible chat endpoint whose replies are Devin's, for
// a client that takes models but speaks no ACP to agents — OpenCode, which drives no ACP agent
// (its ACP client mode was closed as not planned) but takes any OpenAI-compatible provider. An
// OpenCode subagent whose model is this one is a Devin session, shown in OpenCode's own subagent
// interface, which Claude starts there through OpenCode's subagent tool.
//
// What a turn streams: Devin's top agent's text as content and its thoughts as
// `reasoning_content`, and a line for each tool call it makes and each subagent it starts, since a
// chat model can only say what it did. Devin is told to do the work itself; a subagent it starts
// anyway is the two lines, its start and its summary, never sessions of the client's.
//
// The client sends the whole conversation every turn, as chat clients do, so a conversation is
// known by the session OpenCode says it is (`x-opencode-session-id`), else by its first user
// message and directory, and only what is new reaches Devin: a follow-up is a prompt in the same
// Devin session.
//
// A turn can be steered while it runs: OpenCode holds a message for a busy session until the
// step ends, and a step here is Devin's whole turn, so the server is also an MCP server (`/mcp`)
// whose message_task hands a message to the Devin of an OpenCode session at once. Devin takes a
// prompt in the middle of a turn as the user interrupting it, and the turn's reply goes on with
// what it does then.

// soloInstruction ends the first prompt of every session: Devin here is one subagent of another
// agent's, and its own subagents would be work nobody sees.
const soloInstruction = "\n\n(You are running as a subagent of another agent. Do this task yourself: do not start subagents of your own.)"

// keepalive is how often a turn with nothing new says so, so that no proxy or client between
// gives a long Devin turn up as a dead connection.
const keepalive = 15 * time.Second

var workingDirectory = regexp.MustCompile(`(?m)^\s*Working directory: (.+?)\s*$`)

type server struct {
	d   *devin
	cwd string
	// store is where the conversations are kept, so that a restart of the server still knows
	// which Devin session each is, and loads it again.
	store string

	mu       sync.Mutex
	sessions map[string]*served // a conversation's key → the Devin session it is
}

// served is a conversation of the client's and the Devin session it is.
type served struct {
	r *run
	// users is how many of the conversation's user messages Devin has been given.
	users int
	busy  bool
	// live is whether Devin is in a turn of it, which a message steers.
	live bool
}

// kept is a conversation as the store keeps it.
type kept struct {
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
	Users   int    `json:"users"`
}

// load reads the conversations a previous run of the server kept. Their sessions are loaded
// into Devin again on their next turn.
func (s *server) load() {
	raw, err := os.ReadFile(s.store)
	if err != nil {
		return
	}
	var stored map[string]kept
	if err := json.Unmarshal(raw, &stored); err != nil {
		log.Printf("reading %s: %v", s.store, err)
		return
	}
	for key, k := range stored {
		r := &run{child: "devin:" + k.Session, devinSession: k.Session, cwd: k.Cwd,
			agents: map[string]string{rootAgent: "devin:" + k.Session}}
		s.d.runs[k.Session] = r
		s.sessions[key] = &served{r: r, users: k.Users}
	}
	log.Printf("kept %d conversations from before", len(stored))
}

// save writes the conversations down, through a temporary file so a write cut short leaves the
// last one whole. Called with s.mu held.
func (s *server) save() {
	stored := make(map[string]kept, len(s.sessions))
	for key, conversation := range s.sessions {
		stored[key] = kept{Session: conversation.r.devinSession, Cwd: conversation.r.cwd, Users: conversation.users}
	}
	raw, _ := json.Marshal(stored)
	if err := os.MkdirAll(filepath.Dir(s.store), 0o755); err != nil {
		log.Printf("keeping conversations: %v", err)
		return
	}
	tmp := s.store + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("keeping conversations: %v", err)
		return
	}
	if err := os.Rename(tmp, s.store); err != nil {
		log.Printf("keeping conversations: %v", err)
	}
}

func runServe(listen string, devinArgs []string, idleAfter time.Duration, effort string) {
	cwd, _ := os.Getwd()
	cache, _ := os.UserCacheDir()
	p := &proxy{children: map[string]*run{}, waiting: map[string]chan map[string]any{}}
	s := &server{d: newDevin(devinArgs, p), cwd: cwd, sessions: map[string]*served{},
		store: filepath.Join(cache, "claude-devin-acp", "conversations.json")}
	s.d.idleAfter = idleAfter
	s.d.effort = effort
	p.devin = s.d
	s.load()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.complete)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { serveMCP(w, r, steerTools, s.call) })
	log.Printf("serving Devin as a model on http://%s/v1", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var models []any
	for _, effort := range []string{"medium", "high", "max"} {
		models = append(models, map[string]any{"id": "swe-2-" + effort, "object": "model", "owned_by": "cognition"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

type chatRequest struct {
	Model string `json:"model"`
	// ReasoningEffort is what OpenAI-compatible clients send for a model's effort variant.
	ReasoningEffort string `json:"reasoning_effort"`
	Stream          bool   `json:"stream"`
	Messages        []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// text is a message's content as text: a string, or the text of its parts.
func text(content json.RawMessage) string {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return plain
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &parts)
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func (s *server) complete(w http.ResponseWriter, req *http.Request) {
	var chat chatRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, 64<<20)).Decode(&chat); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var users []string
	var system strings.Builder
	for _, message := range chat.Messages {
		switch message.Role {
		case "user":
			users = append(users, text(message.Content))
		case "system", "developer":
			system.WriteString(text(message.Content))
			system.WriteString("\n")
		}
	}
	if len(users) == 0 {
		http.Error(w, "no user message", http.StatusBadRequest)
		return
	}
	cwd := s.cwd
	if found := workingDirectory.FindStringSubmatch(system.String()); found != nil {
		cwd = found[1]
	}
	key := req.Header.Get("x-opencode-session-id")
	if key == "" {
		sum := sha256.Sum256([]byte(cwd + "\x00" + users[0]))
		key = hex.EncodeToString(sum[:])
	}
	conversation, prompt, err := s.turn(key, cwd, users)
	if err != nil {
		log.Printf("turn refused in %s: %v", cwd, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	log.Printf("turn %d of %s in %s (stream %v)", conversation.users, conversation.r.devinSession, cwd, chat.Stream)
	out := newReply(w, chat.Stream)
	out.cwd = cwd
	defer func() {
		s.mu.Lock()
		conversation.busy, conversation.live = false, false
		s.mu.Unlock()
	}()

	events := make(chan event, 4096)
	r := conversation.r
	r.mu.Lock()
	r.watch = func(owner string, update map[string]any) { events <- event{owner, update} }
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.watch = nil
		r.mu.Unlock()
	}()

	done := make(chan error, 1)
	s.d.busy()
	defer s.d.rested()
	if err := s.d.open(r); err != nil {
		out.line("Devin did not start: " + err.Error())
		out.finish()
		return
	}
	if err := s.d.configure(r, effortOf(chat)); err != nil {
		out.line("Devin did not start: " + err.Error())
		out.finish()
		return
	}
	s.mu.Lock()
	conversation.live = true
	s.mu.Unlock()
	go func() {
		result, err := s.d.request("session/prompt", map[string]any{
			"sessionId": r.devinSession, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
		})
		if err == nil {
			switch stop, _ := result["stopReason"].(string); stop {
			case "", "end_turn":
			case "cancelled":
				err = errors.New("Devin's turn was cancelled")
			default:
				// max_tokens, max_turn_requests, refusal: the turn ended before the work did.
				err = errors.New("Devin stopped early (" + stop + ")")
			}
		}
		done <- err
	}()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	for {
		select {
		case e := <-events:
			out.event(e)
		case <-tick.C:
			out.keepalive()
		case <-req.Context().Done():
			log.Printf("the client gave up turn %d of %s; cancelling it", conversation.users, r.devinSession)
			s.d.notifyDevin("session/cancel", map[string]any{"sessionId": r.devinSession})
			<-done
			return
		case err := <-done:
			for len(events) > 0 {
				out.event(<-events)
			}
			if err != nil {
				out.line("Devin did not finish: " + err.Error())
			}
			out.finish()
			log.Printf("turn %d of %s ended (error: %v)", conversation.users, r.devinSession, err)
			return
		}
	}
}

// turn is the Devin session a request's conversation is, opened on its first turn, and what to
// prompt it with: the user messages it has not been given yet.
func (s *server) turn(key, cwd string, users []string) (*served, string, error) {
	s.mu.Lock()
	conversation := s.sessions[key]
	if conversation != nil && conversation.busy {
		s.mu.Unlock()
		return nil, "", errors.New("Devin is still working on this conversation's last turn")
	}
	if conversation != nil && conversation.users >= len(users) {
		// The same turn asked again (a retry): Devin answers the last message again.
		conversation.users = len(users) - 1
	}
	if conversation != nil {
		conversation.busy = true
	}
	s.mu.Unlock()

	if conversation == nil {
		if err := s.d.start(); err != nil {
			return nil, "", err
		}
		result, err := s.d.request("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
		if err != nil {
			return nil, "", err
		}
		devinSession, _ := result["sessionId"].(string)
		if devinSession == "" {
			return nil, "", errors.New("Devin opened no session")
		}
		r := &run{child: "devin:" + devinSession, devinSession: devinSession, cwd: cwd, loaded: true,
			agents: map[string]string{rootAgent: "devin:" + devinSession}}
		s.d.mu.Lock()
		s.d.runs[devinSession] = r
		s.d.mu.Unlock()
		conversation = &served{r: r, busy: true}
		s.mu.Lock()
		s.sessions[key] = conversation
		s.mu.Unlock()
	}
	fresh := users[conversation.users:]
	prompt := strings.Join(fresh, "\n\n")
	if conversation.users == 0 {
		prompt += soloInstruction
	}
	s.mu.Lock()
	conversation.users = len(users)
	s.save()
	s.mu.Unlock()
	return conversation, prompt, nil
}

const steerTools = `[{
  "name": "message_task",
  "title": "Message a running Devin task",
  "description": "Send a message to Devin while it works on a task you gave it as a background subagent, without waiting for it to finish: to correct it, narrow or widen the task, add what you have learned, or tell it to stop and report. Devin reads it at once, as an interruption, and its reply to the subagent call covers what it did after. Only for a task still running; a finished one is continued with the subagent tool and its sessionID.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "sessionID": {"type": "string", "description": "The subagent session Devin is working in, as the subagent tool gave it."},
      "message": {"type": "string", "description": "What to tell Devin."}
    },
    "required": ["sessionID", "message"]
  }
}]`

// steered is the update a turn's watcher is handed for a message given to Devin mid-turn.
const steered = "_steered"

func (s *server) call(params map[string]any) map[string]any {
	if name, _ := params["name"].(string); name != "message_task" {
		return toolResult("no tool "+name, true)
	}
	args, _ := params["arguments"].(map[string]any)
	session, _ := args["sessionID"].(string)
	message, _ := args["message"].(string)
	if strings.TrimSpace(message) == "" {
		return toolResult("message is required", true)
	}
	s.mu.Lock()
	conversation := s.sessions[session]
	live := conversation != nil && conversation.live
	s.mu.Unlock()
	if conversation == nil {
		return toolResult("no Devin task in session "+session, true)
	}
	if !live {
		return toolResult("Devin is not working on session "+session+" now; continue it with the subagent tool and this sessionID", true)
	}
	r := conversation.r
	r.mu.Lock()
	watch := r.watch
	r.mu.Unlock()
	if watch != nil {
		watch(rootAgent, map[string]any{"sessionUpdate": steered, "message": message})
	}
	// Answered when the turn it joins ends, as the turn's own prompt is; nothing waits for it.
	go func() {
		if _, err := s.d.request("session/prompt", map[string]any{
			"sessionId": r.devinSession, "prompt": []any{map[string]any{"type": "text", "text": message}},
		}); err != nil {
			log.Printf("steering %s: %v", r.devinSession, err)
		}
	}()
	log.Printf("steered %s (%s)", r.devinSession, session)
	return toolResult("Devin has the message and is going on with it; its reply to the subagent call covers what it does now.", false)
}

type event struct {
	owner  string
	update map[string]any
}

// reply writes one turn back, streamed as chunks or gathered into one completion.
type reply struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	stream   bool
	cwd      string // the directory paths in lines are said relative to
	id       string
	started  bool
	atLine   bool // whether what was written last ends a line of its own
	content  strings.Builder
	thoughts strings.Builder
}

func newReply(w http.ResponseWriter, stream bool) *reply {
	o := &reply{w: w, stream: stream, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), atLine: true}
	o.flusher, _ = w.(http.Flusher)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	}
	return o
}

func (o *reply) chunk(delta map[string]any, finish any) {
	if !o.started {
		delta["role"] = "assistant"
		o.started = true
	}
	line, _ := json.Marshal(map[string]any{
		"id": o.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "devin",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	fmt.Fprintf(o.w, "data: %s\n\n", line)
	if o.flusher != nil {
		o.flusher.Flush()
	}
}

func (o *reply) say(s string) {
	if s == "" {
		return
	}
	o.atLine = strings.HasSuffix(s, "\n")
	if o.stream {
		o.chunk(map[string]any{"content": s}, nil)
		return
	}
	o.content.WriteString(s)
}

func (o *reply) think(s string) {
	if s == "" {
		return
	}
	if o.stream {
		o.chunk(map[string]any{"reasoning_content": s}, nil)
		return
	}
	o.thoughts.WriteString(s)
}

// line says what Devin did as a line of its own, set off from the text around it.
func (o *reply) line(s string) {
	if !o.atLine {
		o.say("\n\n")
	}
	o.say(s + "\n\n")
}

func (o *reply) keepalive() {
	if o.stream {
		fmt.Fprint(o.w, ": working\n\n")
		if o.flusher != nil {
			o.flusher.Flush()
		}
	}
}

func (o *reply) event(e event) {
	update := e.update
	meta, _ := update["_meta"].(map[string]any)
	if started, _ := meta[metaStarted].(map[string]any); started != nil {
		title, _ := started["title"].(string)
		profile, _ := started["profile"].(string)
		o.line(fmt.Sprintf("Started a subagent (%s): %s", profile, title))
		return
	}
	if completed, _ := meta[metaCompleted].(map[string]any); completed != nil {
		summary, _ := completed["summary"].(string)
		if success, _ := completed["success"].(bool); success {
			o.line("Subagent finished: " + firstLine(summary))
		} else {
			o.line("Subagent failed: " + firstLine(summary))
		}
		return
	}
	kind, _ := update["sessionUpdate"].(string)
	if kind == steered {
		message, _ := update["message"].(string)
		o.line("Message from the lead agent: " + firstLine(message))
		return
	}
	if kind == permissionRefused {
		what := "Devin asked for a permission"
		if title, _ := update["title"].(string); title != "" {
			what = "Devin asked permission to " + title
		}
		o.line(what + "; refused, with nobody here to ask")
		return
	}
	if e.owner != rootAgent {
		return
	}
	switch kind {
	case "agent_message_chunk":
		if content, _ := update["content"].(map[string]any); content != nil {
			t, _ := content["text"].(string)
			o.say(t)
		}
	case "agent_thought_chunk":
		if content, _ := update["content"].(map[string]any); content != nil {
			t, _ := content["text"].(string)
			o.think(t)
		}
	case "tool_call":
		id, _ := update["toolCallId"].(string)
		if strings.HasPrefix(id, "run_subagent:") || strings.HasPrefix(id, "read_subagent:") {
			return // said by the subagent's own lines
		}
		o.line(toolTitle(update, meta, o.cwd))
	case "tool_call_update":
		if status, _ := update["status"].(string); status == "failed" {
			o.line("Failed: " + toolTitle(update, meta, o.cwd))
		}
	}
}

// toolTitle is a tool call as a line says it: Devin's title for it, and what it was called on
// (a file, a command, a pattern) when the call names one, since "Read file" alone says little.
func toolTitle(update, meta map[string]any, cwd string) string {
	title, _ := update["title"].(string)
	if title == "" || title == "Tool" {
		title, _ = meta["cognition.ai/inferenceToolName"].(string)
	}
	if title == "" {
		title, _ = update["toolCallId"].(string)
	}
	input, _ := update["rawInput"].(map[string]any)
	for _, key := range []string{"file_path", "path", "command", "pattern", "query", "url"} {
		if value, _ := input[key].(string); value != "" && !strings.Contains(title, value) {
			if cwd != "" {
				if rel, err := filepath.Rel(cwd, value); err == nil && !strings.HasPrefix(rel, "..") {
					value = rel
				}
			}
			return title + ": " + firstLine(value)
		}
	}
	return title
}

func (o *reply) finish() {
	if o.stream {
		o.chunk(map[string]any{}, "stop")
		fmt.Fprint(o.w, "data: [DONE]\n\n")
		if o.flusher != nil {
			o.flusher.Flush()
		}
		return
	}
	message := map[string]any{"role": "assistant", "content": strings.TrimSpace(o.content.String())}
	if o.thoughts.Len() > 0 {
		message["reasoning_content"] = o.thoughts.String()
	}
	o.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(o.w).Encode(map[string]any{
		"id": o.id, "object": "chat.completion", "created": time.Now().Unix(), "model": "devin",
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
	})
}

// effortOf is the thinking a request asks Devin for: the model's own (`swe-2-max`, Devin's own
// names for SWE-2 at each effort), else the
// request's `reasoning_effort` where it is one Devin takes (low is taken as medium), else none,
// for the server's default.
func effortOf(chat chatRequest) string {
	if effort, ok := strings.CutPrefix(chat.Model, "swe-2-"); ok && efforts[effort] {
		return effort
	}
	switch chat.ReasoningEffort {
	case "minimal", "low", "medium":
		return "medium"
	case "high":
		return "high"
	case "max", "xhigh":
		return "max"
	}
	return ""
}
