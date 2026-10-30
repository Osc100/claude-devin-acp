package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Devin's dialect, as its CLI 3000.11.234 speaks it (captured 2026-10-30):
//
//   - It reports its subagents only to a client whose `clientCapabilities._meta` holds
//     `cognition.ai/subagentSupport` and `cognition.ai/subagentControl`, both `true`; the RFD's
//     `clientCapabilities.subagents` is ignored.
//   - A subagent's whole run is streamed on the parent's own session id. Every update of it
//     carries `_meta["cognition.ai/subagent_context"].parentAgentId`, which despite its name is
//     the id of the agent the update belongs to; Devin's top agent is "root".
//   - A subagent starts with a `tool_call_update` whose toolCallId is its agent id, carrying
//     `cognition.ai/subagent_started` {agentId, title, task, profile, depth}, and ends with
//     `cognition.ai/subagent_completed` {agentId, success, summary}. Neither is a tool call the
//     editor was told of, so both are turned into the RFD's own messages and not passed on.
const (
	metaContext   = "cognition.ai/subagent_context"
	metaStarted   = "cognition.ai/subagent_started"
	metaCompleted = "cognition.ai/subagent_completed"
	rootAgent     = "root"
	// permissionRefused is the update a run's watcher is handed for a permission Devin asked
	// for with no editor to ask: not one of Devin's own.
	permissionRefused = "_permission_refused"
)

// forwarded are the updates of a Devin session that make sense in a child's transcript. Its mode,
// its commands, its configuration and its usage are Devin's own and would be taken as the child's
// settings by the editor.
var forwarded = map[string]bool{
	"agent_message_chunk": true, "agent_thought_chunk": true, "user_message_chunk": true,
	"tool_call": true, "tool_call_update": true, "plan": true,
}

// devin is the one Devin process every run shares, started on the first run.
type devin struct {
	args []string
	p    *proxy

	mu      sync.Mutex
	in      *wire
	ready   chan struct{} // closed once the running Devin is initialized; nil while none runs
	failed  error
	pending map[string]chan map[string]any
	nextID  int
	runs    map[string]*run // Devin's session id → the run in it

	// idleAfter is how long Devin is kept with no turn running before it is stopped, nought to
	// keep it: a Devin process with a session open holds about 130 MB. A run's session outlives
	// it, saved by Devin, and is loaded again before the run's next turn (`open`).
	idleAfter time.Duration
	// effort is the thinking SWE-2 is given when a turn asks for none: medium, high or max.
	effort string
	proc   *os.Process
	turns  int // turns running now
	idle   *time.Timer
}

// busy notes a turn starting: Devin is not stopped while any runs.
func (d *devin) busy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turns++
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
}

// rested notes a turn ending, and when it was the last, stops Devin once idleAfter passes
// with no other.
func (d *devin) rested() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turns--
	if d.turns > 0 || d.idleAfter <= 0 || d.proc == nil {
		return
	}
	d.idle = time.AfterFunc(d.idleAfter, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.turns == 0 && d.proc != nil {
			log.Printf("Devin idle for %v; stopping it until the next turn", d.idleAfter)
			_ = d.proc.Kill()
		}
	})
}

// sweModel is the one model a Devin run is: SWE-2. Through ACP Devin offers it as this one id,
// and how hard it thinks is its `thought_level` (SWE-2 "Max" is this model at `max`).
const sweModel = "swe-2-high"

// efforts are the thinking levels Devin takes for SWE-2.
var efforts = map[string]bool{"medium": true, "high": true, "max": true}

// configure sets r's session to SWE-2 at effort (the default when empty), unless it already is.
// The model first: setting it puts the thinking back to the model's own default.
func (d *devin) configure(r *run, effort string) error {
	if effort == "" {
		effort = d.effort
	}
	if !efforts[effort] {
		return fmt.Errorf("no effort %q: medium, high or max", effort)
	}
	r.mu.Lock()
	set := r.effort == effort
	r.mu.Unlock()
	if set {
		return nil
	}
	for _, option := range [][2]string{{"model", sweModel}, {"thought_level", effort}} {
		if _, err := d.request("session/set_config_option", map[string]any{
			"sessionId": r.devinSession, "configId": option[0], "value": option[1],
		}); err != nil {
			return fmt.Errorf("setting Devin's %s: %w", option[0], err)
		}
	}
	r.mu.Lock()
	r.effort = effort
	r.mu.Unlock()
	return nil
}

// open has Devin running and holding r's session: loaded again if this Devin process has not
// opened it, as after an idle stop or a restart. What Devin replays of the session while it
// loads is history the client has, and is passed on to nobody.
func (d *devin) open(r *run) error {
	if err := d.start(); err != nil {
		return err
	}
	r.mu.Lock()
	loaded := r.loaded
	r.mu.Unlock()
	if loaded {
		return nil
	}
	r.mu.Lock()
	r.replaying = true
	r.mu.Unlock()
	_, err := d.request("session/load", map[string]any{
		"sessionId": r.devinSession, "cwd": r.cwd, "mcpServers": []any{},
	})
	r.mu.Lock()
	r.replaying = false
	r.loaded = err == nil
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("reopening Devin session %s: %w", r.devinSession, err)
	}
	log.Printf("reopened Devin session %s", r.devinSession)
	return nil
}

func newDevin(args []string, p *proxy) *devin {
	return &devin{args: args, p: p, pending: map[string]chan map[string]any{}, runs: map[string]*run{}}
}

// run is one Devin session, shown to the editor as a child of the Claude session that asked for
// it, its subagents as the child's children.
type run struct {
	parent, child, devinSession string
	shown                       bool

	mu     sync.Mutex
	agents map[string]string // Devin's agent id → the editor's session id for it
	answer strings.Builder   // what Devin's top agent said this turn, which goes back to Claude
	broken bool              // whether Devin did something else since `answer` last grew
	title  string            // the label of its latest turn
	// watch, when set, is handed every update of the run with the agent it belongs to, before
	// anything else is done with it: how `serve` streams a run to a client that is no editor.
	watch func(owner string, update map[string]any)
	// cwd is the directory its session was opened in, which loading it again names.
	cwd string
	// loaded is whether the running Devin process holds its session; replaying, whether it is
	// replaying it as it loads, when nothing it says is new.
	loaded, replaying bool
	// effort is the thinking its session was last set to, empty when it is not known to be set
	// (a new process, a session loaded again).
	effort string

	// Its latest turn: done is closed when it ends, outcome and failure are then what it said
	// and what went wrong.
	done     chan struct{}
	finished bool
	outcome  string
	failure  error
	prompts  int
}

func (r *run) sessionOf(agent string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, ok := r.agents[agent]; ok {
		return session
	}
	return r.child
}

func (d *devin) start() error {
	d.mu.Lock()
	if ready := d.ready; ready != nil {
		d.mu.Unlock()
		<-ready
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.failed
	}
	ready := make(chan struct{})
	d.ready, d.failed = ready, nil
	fail := func(err error) error {
		d.failed, d.ready = err, nil
		close(ready)
		d.mu.Unlock()
		return err
	}
	cmd := exec.Command(d.args[0], d.args[1:]...)
	cmd.Stderr = devinLog()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("starting Devin (%s): %w", strings.Join(d.args, " "), err))
	}
	d.in = &wire{w: stdin}
	d.proc = cmd.Process
	d.mu.Unlock()
	go func() {
		d.p.pump(stdout, d.fromDevin)
		_ = cmd.Wait()
		d.mu.Lock()
		d.ready, d.in, d.proc = nil, nil, nil
		for _, r := range d.runs {
			r.mu.Lock()
			r.loaded, r.effort = false, ""
			r.mu.Unlock()
		}
		for id, answer := range d.pending {
			answer <- map[string]any{"error": map[string]any{"message": "Devin exited"}}
			delete(d.pending, id)
		}
		d.mu.Unlock()
		log.Print("Devin exited; the next run starts it again")
	}()
	// Every run waits on `ready`, so nothing reaches Devin before it is initialized.
	_, err = d.request("initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false,
			"_meta": map[string]any{"cognition.ai/subagentSupport": true, "cognition.ai/subagentControl": true},
		},
	})
	d.mu.Lock()
	if err != nil {
		d.failed = fmt.Errorf("initializing Devin: %w", err)
	}
	close(ready)
	d.mu.Unlock()
	return err
}

// devinLog is where Devin's own log goes: it writes a great deal to stderr, which would bury the
// editor's log of this agent.
func devinLog() *os.File {
	dir, err := os.UserCacheDir()
	if err == nil {
		dir = filepath.Join(dir, "claude-devin-acp")
		err = os.MkdirAll(dir, 0o755)
	}
	if err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "devin.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			return f
		}
	}
	return os.Stderr
}

func (d *devin) request(method string, params any) (map[string]any, error) {
	d.mu.Lock()
	in := d.in
	if in == nil {
		d.mu.Unlock()
		return nil, errors.New("Devin is not running")
	}
	d.nextID++
	id := d.nextID
	answer := make(chan map[string]any, 1)
	d.pending[idKey(id)] = answer
	d.mu.Unlock()
	in.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	msg := <-answer
	if e, ok := msg["error"].(map[string]any); ok {
		return nil, fmt.Errorf("Devin refused %s: %v", method, e["message"])
	}
	result, _ := msg["result"].(map[string]any)
	return result, nil
}

// reply answers a request of Devin's, if the Devin that asked is still running.
func (d *devin) reply(msg map[string]any) {
	d.mu.Lock()
	in := d.in
	d.mu.Unlock()
	if in != nil {
		in.send(msg)
	}
}

func (d *devin) notifyDevin(method string, params any) {
	d.mu.Lock()
	in := d.in
	d.mu.Unlock()
	if in != nil {
		in.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
}

func (d *devin) fromDevin(line []byte) {
	msg, ok := decode(line)
	if !ok {
		return
	}
	method, _ := msg["method"].(string)
	switch {
	case method == "" && msg["id"] != nil:
		key := idKey(msg["id"])
		d.mu.Lock()
		answer, ok := d.pending[key]
		delete(d.pending, key)
		d.mu.Unlock()
		if ok {
			answer <- msg
		}
	case method == "session/update":
		params, _ := msg["params"].(map[string]any)
		d.update(params, nil)
	case method == "session/request_permission":
		go d.askPermission(msg)
	case method == "_cognition.ai/agent_stopped":
		// Devin's own word on which model a turn ran on: the check that a run is SWE-2 at the
		// effort it was set to.
		params, _ := msg["params"].(map[string]any)
		stats, _ := params["stats"].(map[string]any)
		log.Printf("a Devin turn ended on %v", stats["modelLabel"])
	case msg["id"] != nil:
		d.reply(map[string]any{"jsonrpc": "2.0", "id": msg["id"],
			"error": map[string]any{"code": -32601, "message": "not offered by claude-devin-acp"}})
	}
	// Devin's `_cognition.ai/*` notifications are its own and go nowhere.
}

func (d *devin) runOf(devinSession string) *run {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs[devinSession]
}

func ownerOf(meta map[string]any) string {
	if context, _ := meta[metaContext].(map[string]any); context != nil {
		if owner, _ := context["parentAgentId"].(string); owner != "" {
			return owner
		}
	}
	return rootAgent
}

// update translates one of Devin's session updates for the run it belongs to (looked up when r is
// nil): a subagent's start and end become the RFD's announcements, and its traffic its own session.
func (d *devin) update(params map[string]any, r *run) {
	devinSession, _ := params["sessionId"].(string)
	if r == nil {
		r = d.runOf(devinSession)
	}
	update, _ := params["update"].(map[string]any)
	if r == nil || update == nil {
		return
	}
	meta, _ := update["_meta"].(map[string]any)
	owner := ownerOf(meta)
	r.mu.Lock()
	watch, replaying := r.watch, r.replaying
	r.mu.Unlock()
	if replaying {
		return
	}
	if watch != nil {
		watch(owner, update)
	}
	if started, _ := meta[metaStarted].(map[string]any); started != nil {
		agent, _ := started["agentId"].(string)
		title, _ := started["title"].(string)
		task, _ := started["task"].(string)
		profile, _ := started["profile"].(string)
		parent := r.sessionOf(owner)
		session := r.child + "/" + agent
		r.mu.Lock()
		r.agents[agent] = session
		r.mu.Unlock()
		if r.shown {
			// Devin has a way to stop one subagent (`subagentControl`), but not one this proxy
			// has seen used, so no Stop is offered for them.
			d.p.announce(parent, session, title, "Devin subagent ("+profile+")", false, task, session+":task")
		}
		return
	}
	if completed, _ := meta[metaCompleted].(map[string]any); completed != nil {
		agent, _ := completed["agentId"].(string)
		success, _ := completed["success"].(bool)
		session := r.sessionOf(agent)
		if r.shown && session != r.child {
			parent := r.parentOf(session)
			if success {
				d.p.state(parent, session, idle("end_turn", ""))
			} else {
				summary, _ := completed["summary"].(string)
				d.p.state(parent, session, idle("error", summary))
			}
		}
		return
	}
	kind, _ := update["sessionUpdate"].(string)
	if owner == rootAgent && forwarded[kind] {
		r.mu.Lock()
		if kind != "agent_message_chunk" {
			// Anything between two stretches of text (a thought, a tool call) ends a message;
			// without a break, the next one runs on from the last one's final word.
			r.broken = true
		} else if content, _ := update["content"].(map[string]any); content != nil {
			if text, _ := content["text"].(string); text != "" {
				if r.broken && r.answer.Len() > 0 {
					r.answer.WriteString("\n\n")
				}
				r.broken = false
				r.answer.WriteString(text)
			}
		}
		r.mu.Unlock()
	}
	if r.shown && forwarded[kind] {
		d.p.notify(r.sessionOf(owner), update)
	}
}

// parentOf is the editor session a nested child was announced under: everything up to its last
// `/`, since a child's id is its parent's id and its own Devin agent id.
func (r *run) parentOf(session string) string {
	if i := strings.LastIndex(session, "/"); i > 0 {
		return session[:i]
	}
	return r.parent
}

// askPermission passes a permission Devin asks for to the editor, as asked by the child whose tool
// call it is, and passes the editor's answer back.
func (d *devin) askPermission(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	devinSession, _ := params["sessionId"].(string)
	r := d.runOf(devinSession)
	reply := map[string]any{"jsonrpc": "2.0", "id": msg["id"]}
	if r == nil {
		reply["result"] = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		d.reply(reply)
		return
	}
	call, _ := params["toolCall"].(map[string]any)
	meta, _ := call["_meta"].(map[string]any)
	if d.p.editor == nil {
		// No editor, so nobody to ask (`serve`): refused, and the run's watcher is told.
		reply["result"] = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		d.reply(reply)
		r.mu.Lock()
		watch := r.watch
		r.mu.Unlock()
		if watch != nil {
			title, _ := call["title"].(string)
			watch(ownerOf(meta), map[string]any{"sessionUpdate": permissionRefused, "title": title})
		}
		return
	}
	target := r.parent
	if r.shown {
		target = r.sessionOf(ownerOf(meta))
	}
	params["sessionId"] = target
	answer := d.p.ask("session/request_permission", params)
	if result, ok := answer["result"]; ok {
		reply["result"] = result
	} else {
		reply["error"] = answer["error"]
	}
	d.reply(reply)
}

// begin hands Devin a task in the run `cont` names, or in a new session, and returns the run at
// once: Devin works in the background, its card running, and the run's turn is collected with
// wait. Claude is never held on a call while Devin works, as Claude Code's own background agents
// do not hold it.
func (d *devin) begin(parent, task, title, cwd, cont, effort string) (*run, error) {
	if err := d.start(); err != nil {
		return nil, err
	}
	var r *run
	if cont != "" {
		d.p.mu.Lock()
		r = d.p.children[cont]
		d.p.mu.Unlock()
		if r == nil || r.parent != parent {
			return nil, fmt.Errorf("no Devin run %q in this session to continue", cont)
		}
		if err := d.open(r); err != nil {
			return nil, err
		}
	} else {
		if cwd == "" {
			cwd = d.p.cwdOf(parent)
		}
		result, err := d.request("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
		if err != nil {
			return nil, err
		}
		devinSession, _ := result["sessionId"].(string)
		if devinSession == "" {
			return nil, errors.New("Devin opened no session")
		}
		r = &run{parent: parent, child: "devin:" + devinSession, devinSession: devinSession, cwd: cwd,
			loaded: true, shown: d.p.showsSubagents(), agents: map[string]string{rootAgent: "devin:" + devinSession}}
		d.mu.Lock()
		d.runs[devinSession] = r
		d.mu.Unlock()
		d.p.mu.Lock()
		d.p.children[r.child] = r
		d.p.mu.Unlock()
	}
	if err := d.configure(r, effort); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.done != nil && !r.finished {
		r.mu.Unlock()
		return nil, fmt.Errorf("Devin run %s is still working; wait for it before following up", r.child)
	}
	r.answer.Reset()
	r.broken, r.finished, r.outcome, r.failure = false, false, "", nil
	r.done = make(chan struct{})
	r.prompts++
	message := fmt.Sprintf("%s:prompt:%d", r.child, r.prompts)
	r.title = title
	if r.title == "" {
		r.title = firstLine(task)
	}
	r.mu.Unlock()
	if r.shown {
		d.p.announce(parent, r.child, "Devin: "+r.title, "Devin CLI", true, task, message)
	}
	go d.turn(r, task)
	return r, nil
}

// turn is one of Devin's turns in r, from the prompt to its end: the card's end, and the outcome
// wait hands Claude.
func (d *devin) turn(r *run, task string) {
	d.busy()
	defer d.rested()
	result, err := d.request("session/prompt", map[string]any{
		"sessionId": r.devinSession, "prompt": []any{map[string]any{"type": "text", "text": task}},
	})
	stop, _ := result["stopReason"].(string)
	if err == nil && stop == "cancelled" {
		err = errors.New("Devin's turn was cancelled")
	}
	if r.shown {
		switch {
		case stop == "cancelled":
			d.p.state(r.parent, r.child, idle("cancelled", ""))
		case err != nil:
			d.p.state(r.parent, r.child, idle("error", err.Error()))
		case stop == "":
			d.p.state(r.parent, r.child, idle("end_turn", ""))
		default:
			d.p.state(r.parent, r.child, idle(stop, ""))
		}
	}
	r.mu.Lock()
	r.outcome = strings.TrimSpace(r.answer.String())
	r.failure = err
	r.finished = true
	close(r.done)
	r.mu.Unlock()
}

// report is what wait says of a run: its id and title, and its answer once its turn is over.
func (r *run) report() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	head := fmt.Sprintf("## %s (%s)", r.child, r.title)
	switch {
	case r.done == nil:
		return head + ": never started"
	case !r.finished:
		return head + ": still working"
	case r.failure != nil:
		text := head + ": did not finish: " + r.failure.Error()
		if r.outcome != "" {
			text += "\n\nWhat it said before that:\n" + r.outcome
		}
		return text
	case r.outcome == "":
		return head + ": finished, without a message"
	default:
		return head + ": finished\n\n" + r.outcome
	}
}

// runsOf are the runs of a Claude session that ids name, or all of them with none named.
func (p *proxy) runsOf(parent string, ids []string) ([]*run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var runs []*run
	if len(ids) == 0 {
		for _, r := range p.children {
			if r.parent == parent {
				runs = append(runs, r)
			}
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i].child < runs[j].child })
		return runs, nil
	}
	for _, id := range ids {
		r := p.children[id]
		if r == nil || r.parent != parent {
			return nil, fmt.Errorf("no Devin run %q in this session", id)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// wait holds until every run is done, ctx ends or the timeout passes, whichever is first.
func wait(ctx context.Context, runs []*run, timeout time.Duration) {
	deadline := time.After(timeout)
	for _, r := range runs {
		r.mu.Lock()
		done := r.done
		r.mu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// cancelChild stops the Devin run the editor's child session is, and says whether the session was
// one of the proxy's, which Claude must never be asked about.
func (p *proxy) cancelChild(session string) bool {
	if !strings.HasPrefix(session, "devin:") {
		return false
	}
	p.mu.Lock()
	r := p.children[session]
	p.mu.Unlock()
	if r != nil {
		p.devin.notifyDevin("session/cancel", map[string]any{"sessionId": r.devinSession})
	}
	return true
}

// announce tells the editor a child is running under parent, and lays the task down as a message
// from the parent to it, in both transcripts: the editor draws the child's card where the
// parent's message to it is.
func (p *proxy) announce(parent, child, title, description string, cancellable bool, task, messageID string) {
	capabilities := map[string]any{}
	if cancellable {
		capabilities["cancel"] = map[string]any{}
	}
	p.notify(parent, map[string]any{
		"sessionUpdate": "subagent_update", "sessionId": child,
		"title": title, "description": description,
		"capabilities": capabilities, "state": map[string]any{"state": "running"},
	})
	message := map[string]any{
		"sessionUpdate": "session_message", "messageId": messageID,
		"senderSessionId": parent, "recipientSessionId": child,
		"content": []any{map[string]any{"type": "text", "text": task}},
	}
	outgoing, incoming := clone(message), clone(message)
	outgoing["recipientMessageId"] = messageID
	incoming["senderMessageId"] = messageID
	p.notify(parent, outgoing)
	p.notify(child, incoming)
}

func (p *proxy) state(parent, child string, state map[string]any) {
	p.notify(parent, map[string]any{"sessionUpdate": "subagent_update", "sessionId": child, "state": state})
}

// idle is the RFD's end of a child's work: `error` carries what went wrong, every other stop
// reason only itself.
func idle(stop, problem string) map[string]any {
	state := map[string]any{"state": "idle", "stopReason": stop}
	if stop == "error" && problem != "" {
		state["error"] = map[string]any{"code": -32603, "message": problem}
	}
	return state
}

func clone(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
