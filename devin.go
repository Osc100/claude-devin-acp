package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Devin's dialect, as its CLI 3000.11.234 speaks it (captured 2026-10-30):
//
//   - It reports its subagents only to a client whose `clientCapabilities._meta` holds
//     `cognition.ai/subagentSupport` and `cognition.ai/subagentControl`, both `true`.
//   - A subagent's whole run is streamed on the parent's own session id. Every update of it
//     carries `_meta["cognition.ai/subagent_context"].parentAgentId`, which despite its name is
//     the id of the agent the update belongs to; Devin's top agent is "root".
//   - A subagent starts with a `tool_call_update` whose toolCallId is its agent id, carrying
//     `cognition.ai/subagent_started` {agentId, title, task, profile, depth}, and ends with
//     `cognition.ai/subagent_completed` {agentId, success, summary}. A reply says each as a line
//     (`serve.go`).
const (
	metaContext   = "cognition.ai/subagent_context"
	metaStarted   = "cognition.ai/subagent_started"
	metaCompleted = "cognition.ai/subagent_completed"
	rootAgent     = "root"
	// permissionRefused is the update a run's watcher is handed for a permission Devin asked
	// for with nobody to decide it: not one of Devin's own.
	permissionRefused = "_permission_refused"
	// permissionReviewed is the one handed for a permission the reviewer decided.
	permissionReviewed = "_permission_reviewed"
	// turnStopped is the one handed for Devin's account of a turn it ended: its stats.
	turnStopped = "_turn_stopped"
	// thinkingDone is the one handed when Devin ends a block of thinking.
	thinkingDone = "_thinking_done"
)

// devin is the one Devin process every run shares, started on the first run.
type devin struct {
	args []string

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
	// longLimits are the last rate limit too long to wait out on each model (SWE-2's is the free
	// models' limit, GPT-6.1 Terra's a paid one), which new turns on that model are answered with
	// until it lifts.
	longLimits map[string]limit
	// lastResume is when the last session waiting out a rate limit is to go on (`resumeSlot`).
	lastResume time.Time
	// reviewer, when set, decides the permissions Devin asks for.
	reviewer *reviewer
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

// terraModel is the one other model a turn may ask for, by Devin's own id: GPT-6.1 Terra, always
// at medium.
const terraModel = "gpt-6-1-terra-medium"

// modelName is a Devin model's name as it is said.
func modelName(model string) string {
	if model == terraModel {
		return "GPT-6.1 Terra"
	}
	return "SWE-2"
}

// efforts are the thinking levels Devin takes for SWE-2.
var efforts = map[string]bool{"medium": true, "high": true, "max": true}

// configure sets r's session to model (SWE-2 when empty) at effort (the default when empty; GPT-6.1
// Terra is always at medium), unless it already is. The model first: setting it puts the thinking
// back to the model's own default.
func (d *devin) configure(r *run, model, effort string) error {
	switch model {
	case "":
		model = sweModel
	case sweModel:
	case terraModel:
		effort = "medium"
	default:
		return fmt.Errorf("no model %q: %s or %s", model, sweModel, terraModel)
	}
	if effort == "" {
		effort = d.effort
	}
	if !efforts[effort] {
		return fmt.Errorf("no effort %q: medium, high or max", effort)
	}
	setting := model + " " + effort
	r.mu.Lock()
	set := r.setting == setting
	r.mu.Unlock()
	if set {
		return nil
	}
	for _, option := range [][2]string{{"model", model}, {"thought_level", effort}} {
		if _, err := d.request("session/set_config_option", map[string]any{
			"sessionId": r.devinSession, "configId": option[0], "value": option[1],
		}); err != nil {
			return fmt.Errorf("setting Devin's %s: %w", option[0], err)
		}
	}
	r.mu.Lock()
	r.setting = setting
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

func newDevin(args []string) *devin {
	return &devin{args: args, pending: map[string]chan map[string]any{}, runs: map[string]*run{}, longLimits: map[string]limit{}}
}

// run is one Devin session: a conversation of OpenCode's (`serve.go`).
type run struct {
	devinSession string

	mu sync.Mutex
	// task is what it was first asked, which the permission reviewer is told.
	task string
	// calls are its tool calls in progress as Devin described them, by id: a permission request
	// names only the call's id.
	calls map[string]map[string]any
	// watch, when set, is handed every update of the run with the agent it belongs to, before
	// anything else is done with it: how a turn is streamed back.
	watch func(owner string, update map[string]any)
	// cwd is the directory its session was opened in, which loading it again names.
	cwd string
	// loaded is whether the running Devin process holds its session; replaying, whether it is
	// replaying it as it loads, when nothing it says is new.
	loaded, replaying bool
	// setting is the model and thinking its session was last set to, empty when it is not known
	// to be set (a new process, a session loaded again).
	setting string
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
		pump(stdout, d.fromDevin)
		_ = cmd.Wait()
		d.mu.Lock()
		d.ready, d.in, d.proc = nil, nil, nil
		for _, r := range d.runs {
			r.mu.Lock()
			r.loaded, r.setting = false, ""
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
// server's own log.
func devinLog() *os.File {
	if dir, err := cacheDir(); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "devin.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
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
		code, _ := e["code"].(float64)
		message, _ := e["message"].(string)
		return nil, &acpError{method: method, code: int(code), message: message}
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
		// Devin's own word on which model a turn ran on (the check that a run is the model and
		// effort it was set to), and what the turn did.
		params, _ := msg["params"].(map[string]any)
		stats, _ := params["stats"].(map[string]any)
		log.Printf("a Devin turn ended on %v", stats["modelLabel"])
		d.tell(params, map[string]any{"sessionUpdate": turnStopped, "stats": stats, "cause": params["cause"]})
	case method == "_cognition.ai/thinking_complete":
		params, _ := msg["params"].(map[string]any)
		d.tell(params, map[string]any{"sessionUpdate": thinkingDone})
	case msg["id"] != nil:
		d.reply(map[string]any{"jsonrpc": "2.0", "id": msg["id"],
			"error": map[string]any{"code": -32601, "message": "not offered by claude-devin-acp"}})
	}
	// Devin's `_cognition.ai/*` notifications are its own and go nowhere.
}

// tell hands the watcher of the run a notification of Devin's names an update of the server's own.
func (d *devin) tell(params, update map[string]any) {
	devinSession, _ := params["sessionId"].(string)
	r := d.runOf(devinSession)
	if r == nil {
		return
	}
	r.mu.Lock()
	watch, replaying := r.watch, r.replaying
	r.mu.Unlock()
	if watch != nil && !replaying {
		watch(rootAgent, update)
	}
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

// update hands one of Devin's session updates to the watcher of the run it belongs to (looked up
// when r is nil), after noting the tool call it describes.
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
	r.mu.Lock()
	watch, replaying := r.watch, r.replaying
	r.mu.Unlock()
	if replaying {
		return
	}
	if kind, _ := update["sessionUpdate"].(string); kind == "tool_call" || kind == "tool_call_update" {
		r.noteCall(update)
	}
	if watch != nil {
		watch(ownerOf(meta), update)
	}
}

// noteCall keeps what Devin says of a tool call until it is over.
func (r *run) noteCall(update map[string]any) {
	id, _ := update["toolCallId"].(string)
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if status, _ := update["status"].(string); status == "completed" || status == "failed" {
		delete(r.calls, id)
		return
	}
	if r.calls == nil {
		r.calls = map[string]map[string]any{}
	}
	call := r.calls[id]
	if call == nil {
		call = map[string]any{}
		r.calls[id] = call
	}
	for key, value := range update {
		if key != "sessionUpdate" && value != nil {
			call[key] = value
		}
	}
}

// callOf is a tool call as Devin described it, with what a permission request adds.
func (r *run) callOf(asked map[string]any) map[string]any {
	id, _ := asked["toolCallId"].(string)
	call := map[string]any{}
	r.mu.Lock()
	for key, value := range r.calls[id] {
		call[key] = value
	}
	r.mu.Unlock()
	for key, value := range asked {
		if value != nil {
			call[key] = value
		}
	}
	return call
}

// askPermission answers a permission Devin asks for: a call that only reads is allowed, any other
// is decided by the permission reviewer, and with no reviewer it is refused.
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
	asked, _ := params["toolCall"].(map[string]any)
	call := r.callOf(asked)
	meta, _ := call["_meta"].(map[string]any)
	if v := d.reviewer; v != nil {
		if !v.is(devinSession) {
			if readOnly(call) {
				// Only reads: allowed without asking, and without a word to anyone.
				options, _ := params["options"].([]any)
				reply["result"] = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": chosen(options, true)}}
				d.reply(reply)
				return
			}
			d.reviewed(msg["id"], r, params, call, v.review(r, call))
			return
		}
		if ownTool(call) {
			// The reviewer's one tool, which it asks to call like any other: allowed for the
			// session, so it is not asked again.
			options, _ := params["options"].([]any)
			option := chosen(options, true)
			for _, offered := range options {
				if offered, _ := offered.(map[string]any); offered["optionId"] == "allow_server_session" {
					option = "allow_server_session"
				}
			}
			reply["result"] = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}
			d.reply(reply)
			return
		}
		raw, _ := json.Marshal(params)
		log.Printf("refusing the permission reviewer's own request: %.1500s", raw)
	}
	// Nobody to decide it, or it is the reviewer's own: refused, and the run's watcher is told.
	reply["result"] = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	d.reply(reply)
	r.mu.Lock()
	watch := r.watch
	r.mu.Unlock()
	if watch != nil {
		title, _ := call["title"].(string)
		watch(ownerOf(meta), map[string]any{"sessionUpdate": permissionRefused, "title": title})
	}
}

// reviewed answers a permission request as the reviewer decided it, tells the run's watcher, and
// tells the asking session why it was denied, which no answer to a request can say.
func (d *devin) reviewed(id any, r *run, params, call map[string]any, v verdict) {
	options, _ := params["options"].([]any)
	outcome := map[string]any{"outcome": "cancelled"}
	if option := chosen(options, v.allow); option != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": option}
	}
	d.reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": outcome}})
	meta, _ := call["_meta"].(map[string]any)
	r.mu.Lock()
	watch, cwd := r.watch, r.cwd
	r.mu.Unlock()
	title := toolTitle(call, meta, cwd)
	if v.allow && v.remembered {
		return // allowed before, and said then
	}
	log.Printf("permission for %s in %s: allowed %v (%s)", title, r.devinSession, v.allow, v.reason)
	if watch != nil {
		watch(ownerOf(meta), map[string]any{"sessionUpdate": permissionReviewed, "title": title,
			"allow": v.allow, "reason": v.reason, "remembered": v.remembered})
	}
	if !v.allow {
		// Devin waits on the answer mid-turn, so this joins that turn.
		go func() {
			_, _ = d.request("session/prompt", map[string]any{"sessionId": r.devinSession, "prompt": []any{
				map[string]any{"type": "text", "text": "(The permission reviewer denied " + title + ": " + v.reason + ")"},
			}})
		}()
	}
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
