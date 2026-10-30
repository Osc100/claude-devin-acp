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
// known by its first user message and directory, and only what is new reaches Devin: a follow-up
// is a prompt in the same Devin session.

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

	mu       sync.Mutex
	sessions map[string]*served // a conversation's key → the Devin session it is
}

// served is a conversation of the client's and the Devin session it is.
type served struct {
	r *run
	// users is how many of the conversation's user messages Devin has been given.
	users int
	busy  bool
}

func runServe(listen string, devinArgs []string) {
	cwd, _ := os.Getwd()
	p := &proxy{children: map[string]*run{}, waiting: map[string]chan map[string]any{}}
	s := &server{d: newDevin(devinArgs, p), cwd: cwd, sessions: map[string]*served{}}
	p.devin = s.d
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.complete)
	log.Printf("serving Devin as a model on http://%s/v1", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "devin", "object": "model", "owned_by": "cognition"},
	}})
}

type chatRequest struct {
	Stream   bool `json:"stream"`
	Messages []struct {
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
	conversation, prompt, err := s.turn(cwd, users)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer func() {
		s.mu.Lock()
		conversation.busy = false
		s.mu.Unlock()
	}()

	out := newReply(w, chat.Stream)
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
	go func() {
		result, err := s.d.request("session/prompt", map[string]any{
			"sessionId": r.devinSession, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
		})
		if err == nil {
			if stop, _ := result["stopReason"].(string); stop == "cancelled" {
				err = errors.New("Devin's turn was cancelled")
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
			s.d.notifyDevin("session/cancel", map[string]any{"sessionId": r.devinSession})
			<-done
			return
		case err := <-done:
			for len(events) > 0 {
				out.event(<-events)
			}
			if err != nil {
				out.line("✗ Devin did not finish: " + err.Error())
			}
			out.finish()
			return
		}
	}
}

// turn is the Devin session a request's conversation is, opened on its first turn, and what to
// prompt it with: the user messages it has not been given yet.
func (s *server) turn(cwd string, users []string) (*served, string, error) {
	sum := sha256.Sum256([]byte(cwd + "\x00" + users[0]))
	key := hex.EncodeToString(sum[:])
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
		r := &run{child: "devin:" + devinSession, devinSession: devinSession,
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
	s.mu.Unlock()
	return conversation, prompt, nil
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
		o.line(fmt.Sprintf("⚙ Devin subagent (%s): %s", profile, title))
		return
	}
	if completed, _ := meta[metaCompleted].(map[string]any); completed != nil {
		summary, _ := completed["summary"].(string)
		if success, _ := completed["success"].(bool); success {
			o.line("✓ subagent finished: " + firstLine(summary))
		} else {
			o.line("✗ subagent failed: " + firstLine(summary))
		}
		return
	}
	kind, _ := update["sessionUpdate"].(string)
	if kind == permissionRefused {
		title, _ := update["title"].(string)
		o.line("✗ Devin asked permission to " + title + "; refused, with nobody here to ask")
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
		o.line("⚙ " + toolTitle(update, meta))
	case "tool_call_update":
		if status, _ := update["status"].(string); status == "failed" {
			o.line("✗ " + toolTitle(update, meta) + " failed")
		}
	}
}

// toolTitle is a tool call as a line says it: Devin's title for it, and what it was called on
// (a file, a command, a pattern) when the call names one, since "Read file" alone says little.
func toolTitle(update, meta map[string]any) string {
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
