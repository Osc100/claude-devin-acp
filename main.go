// Command claude-devin-acp is an ACP agent that is Claude with Devin at its side: an editor such
// as Zed starts it in place of the Claude adapter, it passes every message between the editor and
// Claude through unchanged, and it gives Claude one more tool, `devin`, that hands a task to the
// local Devin CLI. Each Devin run is shown to the editor as a subagent of Claude's session, with
// Devin's own subagents nested under it, through ACP's subagents RFD
// (https://agentclientprotocol.com/rfds/subagents).
//
// Why a proxy, not a patch to Claude or to the editor: Claude reaches the tool through an MCP
// server the proxy adds to every session it opens, which ACP already lets a client do, and the
// editor sees only the RFD's own messages. So neither side knows Devin is there, and the proxy is
// the one place that speaks Devin's dialect (`devin.go`).
//
// Claude still runs as published and signs in through Anthropic's own flow; the proxy never sees
// a credential.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultClaude := "node " + filepath.Join(home,
		".local/share/zed/external_agents/registry/npx/claude-acp/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js")
	claudeCmd := flag.String("claude", envOr("CLAUDE_DEVIN_CLAUDE", defaultClaude), "command that starts the Claude ACP adapter")
	devinCmd := flag.String("devin", envOr("CLAUDE_DEVIN_DEVIN", "devin acp"), "command that starts Devin as an ACP agent")
	devinOnly := flag.Bool("devin-only", false, "Devin + Devin: the editor talks to Devin itself, its subagents shown as children")
	serve := flag.String("serve", "", "Devin as a model: serve an OpenAI-compatible chat endpoint on this address (e.g. 127.0.0.1:47821)")
	devinIdle := flag.Duration("devin-idle", 10*time.Minute, "stop Devin after this long with no turn running (0 keeps it); its sessions are loaded again on the next turn")
	claudeSubagents := flag.Bool("claude-subagents", false, "leave Claude its own subagents beside Devin (by default every delegation goes to Devin)")
	flag.Parse()
	log.SetPrefix("claude-devin-acp: ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if *devinOnly {
		runSolo(strings.Fields(*devinCmd))
		return
	}
	if *serve != "" {
		runServe(*serve, strings.Fields(*devinCmd), *devinIdle)
		return
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listening for MCP: %v", err)
	}
	claudeArgs := strings.Fields(*claudeCmd)
	claude := exec.Command(claudeArgs[0], claudeArgs[1:]...)
	claude.Stderr = os.Stderr
	toClaude, err := claude.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	fromClaude, err := claude.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := claude.Start(); err != nil {
		log.Fatalf("starting Claude (%s): %v", *claudeCmd, err)
	}

	p := &proxy{
		editor:            &wire{w: os.Stdout},
		claude:            &wire{w: toClaude},
		mcpBase:           "http://" + listener.Addr().String() + "/mcp/",
		tokens:            map[string]string{},
		cwds:              map[string]string{},
		opening:           map[string]opening{},
		children:          map[string]*run{},
		waiting:           map[string]chan map[string]any{},
		devinOnlyDelegate: !*claudeSubagents,
	}
	p.devin = newDevin(strings.Fields(*devinCmd), p)
	p.devin.idleAfter = *devinIdle
	go func() { log.Fatal(http.Serve(listener, &mcpServer{p: p})) }()
	go func() {
		p.pump(fromClaude, p.fromClaude)
		log.Print("Claude closed its output")
		os.Exit(0)
	}()
	p.pump(os.Stdin, p.fromEditor)
	log.Print("the editor closed its input")
	_ = claude.Process.Kill()
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// wire is one side's input, written a whole line at a time, since ACP over stdio is one JSON-RPC
// message a line and two goroutines may write at once.
type wire struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *wire) send(msg any) {
	line, err := json.Marshal(msg)
	if err != nil {
		log.Printf("encoding a message: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		log.Printf("writing a message: %v", err)
	}
}

// sendRaw writes a line exactly as it came, so a message the proxy has no business with reaches
// the other side byte for byte.
func (w *wire) sendRaw(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		log.Printf("writing a message: %v", err)
	}
}

// opening is a session/new, session/load or session/resume the editor asked of Claude, waiting
// for Claude's answer to learn the session id its MCP token belongs to.
type opening struct {
	token     string
	sessionID string // known up front for a load or a resume, empty for a new session
	cwd       string
}

type proxy struct {
	editor, claude *wire
	devin          *devin
	mcpBase        string

	// Whether every delegation goes to Devin: Claude's own subagent tools are taken from each
	// session it opens and its system prompt says to delegate through `devin` instead.
	devinOnlyDelegate bool

	mu sync.Mutex
	// Whether the editor said it shows subagents; without that a Devin run is still done and
	// answered, but nothing is announced, since the RFD forbids it.
	subagents bool
	tokens    map[string]string  // MCP token → Claude's session id, empty until Claude has answered
	cwds      map[string]string  // Claude's session id → the directory the editor opened it in
	opening   map[string]opening // the editor's request id → the session it opens
	children  map[string]*run    // a child session id the editor sees → the Devin run it is
	waiting   map[string]chan map[string]any
	nextID    int
}

func (p *proxy) pump(r io.Reader, handle func([]byte)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		handle(append([]byte(nil), scanner.Bytes()...))
	}
}

func decode(line []byte) (map[string]any, bool) {
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, false
	}
	return msg, true
}

func idKey(id any) string { return fmt.Sprint(id) }

// fromEditor handles a line the editor wrote.
func (p *proxy) fromEditor(line []byte) {
	msg, ok := decode(line)
	if !ok {
		p.claude.sendRaw(line)
		return
	}
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	switch {
	case method == "" && msg["id"] != nil:
		// An answer: to one of the proxy's own requests, or to Claude's.
		if p.answered(idKey(msg["id"]), msg) {
			return
		}
	case method == "initialize" && params != nil:
		// v1, always: the RFD's v1 form is what the proxy speaks, and an editor that asked for v2
		// starts the agent again over v1 when it is answered with 1.
		if version, _ := params["protocolVersion"].(float64); version > 1 {
			params["protocolVersion"] = 1
		}
		if caps, _ := params["clientCapabilities"].(map[string]any); caps != nil {
			if sub, present := caps["subagents"]; present && sub != nil {
				p.mu.Lock()
				p.subagents = true
				p.mu.Unlock()
			}
		}
		p.claude.send(msg)
		return
	case (method == "session/new" || method == "session/load" || method == "session/resume") && params != nil:
		token := randomToken()
		sessionID, _ := params["sessionId"].(string)
		cwd, _ := params["cwd"].(string)
		servers, _ := params["mcpServers"].([]any)
		params["mcpServers"] = append(servers, map[string]any{
			"type": "http", "name": "devin", "url": p.mcpBase + token, "headers": []any{},
		})
		if p.devinOnlyDelegate {
			delegateToDevin(params)
		}
		p.mu.Lock()
		p.opening[idKey(msg["id"])] = opening{token: token, sessionID: sessionID, cwd: cwd}
		// Known at once, its session not yet: Claude connects to its MCP servers while it opens
		// the session, before its answer says which session that is.
		p.tokens[token] = sessionID
		if sessionID != "" {
			p.cwds[sessionID] = cwd
		}
		p.mu.Unlock()
		p.claude.send(msg)
		return
	case method == "session/cancel" && params != nil:
		if child, _ := params["sessionId"].(string); p.cancelChild(child) {
			return
		}
	}
	p.claude.sendRaw(line)
}

// fromClaude handles a line Claude wrote: always passed on, after noting the session a pending
// session/new opened.
func (p *proxy) fromClaude(line []byte) {
	if msg, ok := decode(line); ok && msg["method"] == nil && msg["id"] != nil {
		key := idKey(msg["id"])
		p.mu.Lock()
		if open, pending := p.opening[key]; pending {
			delete(p.opening, key)
			if result, _ := msg["result"].(map[string]any); result != nil {
				if id, _ := result["sessionId"].(string); id != "" {
					p.tokens[open.token] = id
					p.cwds[id] = open.cwd
				}
			}
		}
		p.mu.Unlock()
	}
	p.editor.sendRaw(line)
}

// ask sends the editor a request of the proxy's own and waits for its answer. The proxy's ids are
// strings Claude never uses, so the editor's answer is told apart from an answer meant for Claude.
func (p *proxy) ask(method string, params any) map[string]any {
	p.mu.Lock()
	p.nextID++
	id := fmt.Sprintf("claude-devin-acp-%d", p.nextID)
	answer := make(chan map[string]any, 1)
	p.waiting[id] = answer
	p.mu.Unlock()
	p.editor.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-answer
}

func (p *proxy) answered(id string, msg map[string]any) bool {
	p.mu.Lock()
	answer, ours := p.waiting[id]
	delete(p.waiting, id)
	p.mu.Unlock()
	if ours {
		answer <- msg
	}
	return ours
}

// notify tells the editor of an update to a session, as an agent does.
func (p *proxy) notify(sessionID string, update map[string]any) {
	p.editor.send(map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": sessionID, "update": update},
	})
}

func (p *proxy) showsSubagents() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.subagents
}

func (p *proxy) sessionOf(token string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.tokens[token]
	return id, ok
}

func (p *proxy) cwdOf(session string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cwds[session]
}

// claudeSubagentTools are Claude Code's own ways to start a subagent: `Agent`, and `Task`, its name
// before. Taken away, Claude has `devin` alone to delegate with.
var claudeSubagentTools = []string{"Agent", "Task"}

const delegationPrompt = `Delegation in this session goes to Devin, a separate coding agent on this machine; your own subagent tools are not available. ` +
	"`mcp__devin__start_task`" + ` starts a Devin run in the background and returns its run id at once: hand Devin any work you would have given a subagent — exploring a codebase, implementing, reviewing, long tasks — as a complete, self-contained task, start independent runs side by side, and keep working meanwhile. Collect answers with ` +
	"`mcp__devin__wait_for_tasks`" + `, and before you end your turn wait for every run you started, unless the user asked you not to. Check what Devin reports before relying on it. Do small, quick steps yourself.`

// delegateToDevin takes Claude's own subagent tools out of a session/new, session/load or
// session/resume and appends to its system prompt that delegation goes to Devin. Both through the
// Claude adapter's `_meta` extensions (claude-agent-acp's NewSessionMeta): `claudeCode.options
// .disallowedTools` is merged with the adapter's own, and `systemPrompt.append` is added to Claude
// Code's preset. Whatever the editor put in either is kept.
func delegateToDevin(params map[string]any) {
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		params["_meta"] = meta
	}
	claudeCode, _ := meta["claudeCode"].(map[string]any)
	if claudeCode == nil {
		claudeCode = map[string]any{}
		meta["claudeCode"] = claudeCode
	}
	options, _ := claudeCode["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
		claudeCode["options"] = options
	}
	disallowed, _ := options["disallowedTools"].([]any)
	for _, tool := range claudeSubagentTools {
		disallowed = append(disallowed, tool)
	}
	options["disallowedTools"] = disallowed

	switch prompt := meta["systemPrompt"].(type) {
	case string:
		// A whole prompt of the editor's own replaces Claude Code's; the note goes at its end.
		meta["systemPrompt"] = prompt + "\n\n" + delegationPrompt
	case map[string]any:
		if appended, _ := prompt["append"].(string); appended != "" {
			prompt["append"] = appended + "\n\n" + delegationPrompt
		} else {
			prompt["append"] = delegationPrompt
		}
	default:
		meta["systemPrompt"] = map[string]any{"append": delegationPrompt}
	}
}
