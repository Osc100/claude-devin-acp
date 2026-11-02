package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The permission reviewer. With nobody at a screen to ask, a permission one Devin session asks
// for is decided by another: a Devin session on SWE-2 at medium, for speed, that does nothing but
// review. It is handed each request with the asking session's task, and answers through
// decide_permission, the one tool of the server's MCP server at /mcp/permissions/<secret>: the
// secret is made each time its session opens and given to that session alone, so nothing else can
// answer for it. Its own requests are refused unasked. It is open while any other session is in a turn, and closed once none is.
//
// Its decisions are kept (permissions.json, beside the conversations): the exact same tool call in
// the same directory is decided again without asking.

const reviewerBrief = `You review permission requests from other coding agents (Devin sessions) working for the user on their machine. Each request comes as a message naming its id, the agent's task and working directory, and the tool call it wants to make. Decide each with the decide_permission tool of the permissions MCP server, exactly once, and do nothing else: do not run commands, read files or start subagents.

Allow what the agent's task plausibly needs that stays within its working directory, build caches or temporary files, or can be undone. Deny what is destructive or irreversible beyond the task (deleting outside its directory, git push, force resets, rewriting history), anything touching credentials, keys or ~/.ssh, sudo or system configuration, sending code or secrets off the machine, and anything unrelated to the task. When unsure, deny, with a reason that tells the agent what to do instead.`

// reviewTimeout is how long a review may take before the request is denied.
const reviewTimeout = 3 * time.Minute

type reviewer struct {
	d   *devin
	url string // its MCP server
	dir string // the directory its session is opened in
	// store is where its decisions are kept.
	store string

	// mu is held through a review, and while its session is opened or closed: one at a time.
	mu      sync.Mutex
	r       *run // its session, nil while closed
	briefed bool // whether its session has been given reviewerBrief

	state   sync.Mutex
	session string // its Devin session id, for telling its own requests apart without mu
	secret  string // the path its session reaches its MCP server at, empty while closed
	live    int    // other sessions in a turn
	nextID  int
	waiting map[string]chan verdict
	decided map[string]decision // a tool call's key → what was decided of it

	mcp http.Handler // its MCP server, which its session reaches
}

type verdict struct {
	allow  bool
	reason string
	// remembered is whether it is a decision kept from before.
	remembered bool
}

// decision is a verdict as the store keeps it, with the call it was for, to be read by a person.
type decision struct {
	Allow  bool      `json:"allow"`
	Reason string    `json:"reason"`
	Call   string    `json:"call"`
	Cwd    string    `json:"cwd"`
	At     time.Time `json:"at"`
}

func newReviewer(d *devin, url, dir, store string) *reviewer {
	v := &reviewer{d: d, url: url, dir: dir, store: store,
		waiting: map[string]chan verdict{}, decided: map[string]decision{}}
	v.mcp = mcpHandler(v.tools())
	if raw, err := os.ReadFile(store); err == nil {
		if err := json.Unmarshal(raw, &v.decided); err != nil {
			log.Printf("reading %s: %v", store, err)
		}
	}
	return v
}

// callKey is what makes two requests the same: the tool call's kind, title and input, and the
// directory it is made in.
func callKey(call map[string]any, cwd string) (string, string) {
	described, _ := json.Marshal(map[string]any{
		"kind": call["kind"], "title": call["title"], "rawInput": call["rawInput"],
	})
	sum := sha256.Sum256(append([]byte(cwd+"\x00"), described...))
	return hex.EncodeToString(sum[:]), string(described)
}

// remember keeps a decision, through a temporary file so a write cut short leaves the last whole.
func (v *reviewer) remember(key string, d decision) {
	v.state.Lock()
	v.decided[key] = d
	raw, _ := json.MarshalIndent(v.decided, "", "  ")
	v.state.Unlock()
	tmp := v.store + ".tmp"
	err := os.WriteFile(tmp, raw, 0o600)
	if err == nil {
		err = os.Rename(tmp, v.store)
	}
	if err != nil {
		log.Printf("keeping a permission decision: %v", err)
	}
}

// hold notes a session's turn starting, and opens the reviewer for the first.
func (v *reviewer) hold() {
	v.state.Lock()
	v.live++
	first := v.live == 1
	v.state.Unlock()
	if first {
		go func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			if err := v.open(); err != nil {
				log.Printf("opening the permission reviewer: %v", err)
			}
		}()
	}
}

// release notes a session's turn ending, and closes the reviewer after the last.
func (v *reviewer) release() {
	v.state.Lock()
	v.live--
	last := v.live == 0
	v.state.Unlock()
	if last {
		go func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			v.state.Lock()
			still := v.live == 0
			v.state.Unlock()
			if still {
				v.close()
			}
		}()
	}
}

func (v *reviewer) is(devinSession string) bool {
	v.state.Lock()
	defer v.state.Unlock()
	return devinSession != "" && devinSession == v.session
}

// open has the reviewer's session open in the running Devin: a new one if Devin was restarted
// since. Called with v.mu held.
func (v *reviewer) open() error {
	if v.r != nil {
		v.r.mu.Lock()
		loaded := v.r.loaded
		v.r.mu.Unlock()
		if loaded {
			return nil
		}
		v.close()
	}
	if err := v.d.start(); err != nil {
		return err
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	secret := randomToken(32)
	result, err := v.d.request("session/new", map[string]any{
		"cwd": v.dir,
		"mcpServers": []any{map[string]any{
			"type": "http", "name": "permissions", "url": v.url + "/" + secret, "headers": []any{},
		}},
	})
	if err != nil {
		return err
	}
	id, _ := result["sessionId"].(string)
	if id == "" {
		return errors.New("Devin opened no session")
	}
	r := &run{devinSession: id, cwd: v.dir, loaded: true}
	v.d.mu.Lock()
	v.d.runs[id] = r
	v.d.mu.Unlock()
	v.state.Lock()
	v.session, v.secret = id, secret
	v.state.Unlock()
	v.r, v.briefed = r, false
	if err := v.d.configure(r, sweModel, "medium"); err != nil {
		v.close()
		return err
	}
	log.Printf("permission reviewer open as %s", id)
	return nil
}

// close closes the reviewer's session. Called with v.mu held.
func (v *reviewer) close() {
	if v.r == nil {
		return
	}
	id := v.r.devinSession
	v.r.mu.Lock()
	loaded := v.r.loaded
	v.r.mu.Unlock()
	v.d.mu.Lock()
	delete(v.d.runs, id)
	v.d.mu.Unlock()
	v.state.Lock()
	v.session, v.secret = "", ""
	v.state.Unlock()
	v.r = nil
	if !loaded {
		return // Devin was stopped since, and the session with it
	}
	if _, err := v.d.request("session/close", map[string]any{"sessionId": id}); err != nil {
		log.Printf("closing the permission reviewer %s: %v", id, err)
		return
	}
	log.Printf("permission reviewer %s closed", id)
}

// review has the reviewer decide a request of asker's, unless the same call was decided before. A
// request it does not decide in time, or ends its turn without deciding, is denied, and that is
// not kept.
func (v *reviewer) review(asker *run, call map[string]any) verdict {
	asker.mu.Lock()
	cwd := asker.cwd
	asker.mu.Unlock()
	key, described := callKey(call, cwd)
	v.state.Lock()
	before, known := v.decided[key]
	v.state.Unlock()
	if known {
		return verdict{before.Allow, before.Reason, true}
	}
	got := v.ask(asker, call)
	if got.decided {
		v.remember(key, decision{got.allow, got.reason, described, cwd, time.Now()})
	}
	return got.verdict
}

// asked is what the reviewer answered, and whether it was its own decision.
type asked struct {
	verdict
	decided bool
}

func (v *reviewer) ask(asker *run, call map[string]any) asked {
	v.mu.Lock()
	if err := v.open(); err != nil {
		v.mu.Unlock()
		return asked{verdict: verdict{reason: "the permission reviewer did not open: " + err.Error()}}
	}
	v.state.Lock()
	v.nextID++
	id := fmt.Sprintf("p%d", v.nextID)
	answer := make(chan verdict, 1)
	v.waiting[id] = answer
	v.state.Unlock()
	defer func() {
		v.state.Lock()
		delete(v.waiting, id)
		v.state.Unlock()
	}()

	prompt := describeRequest(id, asker, call)
	if !v.briefed {
		prompt = reviewerBrief + "\n\n" + prompt
		v.briefed = true
	}
	session := v.r.devinSession
	turn := make(chan error, 1)
	go func() {
		_, err := v.d.request("session/prompt", map[string]any{
			"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
		})
		turn <- err
	}()
	// The next review waits for this one's turn to end, so that each starts a turn of its own.
	settle := func() {
		go func() {
			select {
			case <-turn:
			case <-time.After(time.Minute):
				v.d.notifyDevin("session/cancel", map[string]any{"sessionId": session})
				<-turn
			}
			v.mu.Unlock()
		}()
	}
	timer := time.NewTimer(reviewTimeout)
	defer timer.Stop()
	select {
	case got := <-answer:
		settle()
		return asked{got, true}
	case err := <-turn:
		v.mu.Unlock()
		select {
		case got := <-answer:
			return asked{got, true}
		default:
		}
		if err != nil {
			return asked{verdict: verdict{reason: "the permission reviewer failed: " + err.Error()}}
		}
		return asked{verdict: verdict{reason: "the permission reviewer ended its turn without deciding"}}
	case <-timer.C:
		settle()
		return asked{verdict: verdict{reason: "the permission reviewer did not decide within " + reviewTimeout.String()}}
	}
}

// describeRequest is a request as the reviewer is handed it.
func describeRequest(id string, asker *run, call map[string]any) string {
	asker.mu.Lock()
	task, cwd, session := asker.task, asker.cwd, asker.devinSession
	asker.mu.Unlock()
	if len(task) > 3000 {
		task = task[:3000] + " […]"
	}
	shown := map[string]any{}
	for _, key := range []string{"title", "kind", "rawInput", "locations"} {
		if value, ok := call[key]; ok {
			shown[key] = value
		}
	}
	if meta, _ := call["_meta"].(map[string]any); meta != nil {
		if rule, _ := meta["cognition.ai/permissionRule"].(string); rule != "" {
			shown["permissionRule"] = rule
		}
	}
	var contents []string
	if content, _ := call["content"].([]any); content != nil {
		for _, item := range content {
			raw, _ := json.Marshal(item)
			contents = append(contents, string(raw))
		}
	}
	callJSON, _ := json.MarshalIndent(shown, "", "  ")
	described := string(callJSON)
	if len(contents) > 0 {
		described += "\nContent: " + strings.Join(contents, "\n")
	}
	if len(described) > 6000 {
		described = described[:6000] + " […]"
	}
	return fmt.Sprintf("Permission request %s.\nAgent: Devin session %s, working in %s.\nIts task:\n%s\n\nIt asks permission for this tool call:\n%s\n\nDecide it with decide_permission, id %s.",
		id, session, cwd, task, described, id)
}

// tools is the reviewer's MCP server: decide_permission alone.
func (v *reviewer) tools() *mcp.Server {
	server := newMCP("permissions")
	type decideArgs struct {
		ID     string `json:"id" jsonschema:"The request's id, as its message gives it."`
		Allow  bool   `json:"allow" jsonschema:"true to allow the tool call, false to deny it."`
		Reason string `json:"reason" jsonschema:"One line: why, and for a denial what the agent should do instead."`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "decide_permission", Title: "Decide a permission request",
		Description: "Allow or deny one permission request, by its id. Call it exactly once per request."},
		func(_ context.Context, _ *mcp.CallToolRequest, a decideArgs) (*mcp.CallToolResult, any, error) {
			v.state.Lock()
			answer := v.waiting[a.ID]
			v.state.Unlock()
			if answer == nil {
				return nil, nil, fmt.Errorf("no request %s is waiting", a.ID)
			}
			select {
			case answer <- verdict{allow: a.Allow, reason: strings.TrimSpace(a.Reason)}:
				return said("decided")
			default:
				return nil, nil, fmt.Errorf("request %s is already decided", a.ID)
			}
		})
	return server
}

// ServeHTTP serves the reviewer's MCP server to the one session holding its secret.
func (v *reviewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.state.Lock()
	secret := v.secret
	v.state.Unlock()
	if !sameSecret(r.PathValue("secret"), secret) {
		http.NotFound(w, r)
		return
	}
	v.mcp.ServeHTTP(w, r)
}

// ownTool is whether a tool call is the reviewer's use of its own MCP server, the one thing it
// is let do.
func ownTool(call map[string]any) bool {
	meta, _ := call["_meta"].(map[string]any)
	// A request comes before Devin announces the call, naming it only by the rule it falls under.
	for _, key := range []string{"cognition.ai/permissionRule", "cognition.ai/toolName"} {
		if name, _ := meta[key].(string); strings.HasPrefix(name, "mcp__permissions__") {
			return true
		}
	}
	input, _ := call["rawInput"].(map[string]any)
	server, _ := input["server_name"].(string)
	return server == "permissions"
}

// chosen is the option of a permission request that a verdict picks: one that allows once, or
// rejects once, else any that allows or rejects.
func chosen(options []any, allow bool) string {
	want, prefix := "reject_once", "reject"
	if allow {
		want, prefix = "allow_once", "allow"
	}
	fallback := ""
	for _, option := range options {
		option, _ := option.(map[string]any)
		kind, _ := option["kind"].(string)
		id, _ := option["optionId"].(string)
		if kind == want {
			return id
		}
		if fallback == "" && strings.HasPrefix(kind, prefix) {
			fallback = id
		}
	}
	return fallback
}
