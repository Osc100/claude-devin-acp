package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What `serve` offers about the Devin turns it runs, beside the turns themselves: MCP tools at
// /mcp for Claude to see a task's progress, steer it, and stop it, and the same as JSON at /status
// for `claude-devin-acp status`.

// activity is what a conversation's turns have said, line by line, the last activityKept lines of it.
type activity struct {
	mu      sync.Mutex
	lines   []string
	total   int // lines ever written
	partial strings.Builder
	last    string // its latest line of its own: a tool call, a subagent, a failure
}

const activityKept = 500

func (a *activity) write(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			a.partial.WriteString(s)
			return
		}
		a.partial.WriteString(s[:i])
		if line := a.partial.String(); strings.TrimSpace(line) != "" {
			a.lines = append(a.lines, line)
			a.total++
			if len(a.lines) > activityKept {
				a.lines = a.lines[len(a.lines)-activityKept:]
			}
		}
		a.partial.Reset()
		s = s[i+1:]
	}
}

func (a *activity) did(line string) {
	a.mu.Lock()
	a.last = line
	a.mu.Unlock()
}

// since is the lines from the from-th ever written on (as many as are kept), with the line being
// written when partial is set, and the count to ask from next.
func (a *activity) since(from int, partial bool) ([]string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	first := a.total - len(a.lines)
	if from < first {
		from = first
	}
	var lines []string
	if from < a.total {
		lines = append(lines, a.lines[from-first:]...)
	}
	if partial && strings.TrimSpace(a.partial.String()) != "" {
		lines = append(lines, a.partial.String())
	}
	return lines, a.total
}

// task is a conversation as the status tools give it.
type task struct {
	Session string    `json:"session"`
	Devin   string    `json:"devin"`
	Title   string    `json:"title"`
	Cwd     string    `json:"cwd"`
	Working bool      `json:"working"`
	Since   time.Time `json:"since"` // when its turn started, or ended once it has
	Last    string    `json:"last,omitempty"`
}

func (s *server) taskOf(c *served) task {
	t := task{Session: c.session, Devin: c.r.devinSession, Title: c.title, Cwd: c.r.cwd,
		Working: c.live, Since: c.ended}
	if c.live {
		t.Since = c.started
	}
	c.act.mu.Lock()
	t.Last = c.act.last
	c.act.mu.Unlock()
	return t
}

// tasks are the conversations in a turn and those whose last turn ended within recent, the working
// first, the latest first.
func (s *server) tasks(recent time.Duration) []task {
	s.mu.Lock()
	var tasks []task
	for _, c := range s.sessions {
		if c.live || (!c.ended.IsZero() && time.Since(c.ended) < recent) {
			tasks = append(tasks, s.taskOf(c))
		}
	}
	s.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Working != tasks[j].Working {
			return tasks[i].Working
		}
		return tasks[i].Since.After(tasks[j].Since)
	})
	return tasks
}

// find is the conversation a name is: OpenCode's session id, Devin's, or the start of either when
// only one starts so.
func (s *server) find(name string) (*served, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.sessions[name]; c != nil {
		return c, nil
	}
	var found []*served
	for _, c := range s.sessions {
		if c.r.devinSession == name {
			return c, nil
		}
		if name != "" && (strings.HasPrefix(c.session, name) || strings.HasPrefix(c.r.devinSession, name)) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, fmt.Errorf("no Devin task in session %s", name)
	}
	return nil, fmt.Errorf("%d Devin tasks match %s", len(found), name)
}

func (t task) String() string {
	state := "finished " + ago(t.Since) + " ago"
	if t.Working {
		state = "working for " + ago(t.Since)
	}
	line := fmt.Sprintf("%s (Devin %s): %s\n  %s, in %s", t.Session, t.Devin, t.Title, state, t.Cwd)
	if t.Last != "" {
		line += "\n  last: " + t.Last
	}
	return line
}

func ago(at time.Time) string {
	return time.Since(at).Round(time.Second).String()
}

// steered is the update a turn's watcher is handed for a message given to Devin mid-turn, and
// stopAsked the one for being told to stop.
const (
	steered   = "_steered"
	stopAsked = "_stop_asked"
	// rateLimited is the one handed when a turn waits out a rate limit.
	rateLimited = "_rate_limited"
)

const stopMessage = "Stop now. Do not start anything new; finish or undo only what would otherwise leave files broken. Then reply with a report: what you did, what is left, and the state you are leaving things in."

// devinTools are the tools that watch and steer Devin's tasks (`/mcp`).
func (s *server) devinTools() *mcp.Server {
	server := newMCP("devin")
	mcp.AddTool(server, &mcp.Tool{Name: "task_status", Title: "List Devin tasks",
		Description: "List the Devin tasks running now and those finished in the last hour: each one's sessionID, title, directory, how long it has been working (or since it finished), and the last thing it did. A quick check that needs no waiting; use peek_task for more of one."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			tasks := s.tasks(time.Hour)
			if len(tasks) == 0 {
				return said("no Devin tasks running or finished in the last hour")
			}
			described := make([]string, len(tasks))
			for i, t := range tasks {
				described[i] = t.String()
			}
			return said(strings.Join(described, "\n\n"))
		})
	mcp.AddTool(server, &mcp.Tool{Name: "peek_task", Title: "Peek at a Devin task",
		Description: "Return the latest of what a Devin task has said and done, running or finished: its text and a line for each tool call, subagent and permission, oldest first. To see whether it is on track before steering it with message_task."},
		func(_ context.Context, _ *mcp.CallToolRequest, a peekArgs) (*mcp.CallToolResult, any, error) {
			conversation, err := s.find(a.SessionID)
			if err != nil {
				return nil, nil, err
			}
			lines, _ := conversation.act.since(0, true)
			if n := a.peekLines(); len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			s.mu.Lock()
			t := s.taskOf(conversation)
			s.mu.Unlock()
			return said(t.String() + "\n\n" + strings.Join(lines, "\n"))
		})
	type messageArgs struct {
		SessionID string `json:"sessionID" jsonschema:"The subagent's session, as the subagent tool gave it."`
		Message   string `json:"message" jsonschema:"What to tell Devin."`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "message_task", Title: "Message a running Devin task",
		Description: "Send a message to Devin while it works on a task you gave it as a background subagent, without waiting for it to finish: to correct it, narrow or widen the task, add what you have learned, or tell it to stop and report. Devin reads it at once, as an interruption, and its reply to the subagent call covers what it did after. Only for a task still running; a finished one is continued with the subagent tool and its sessionID."},
		func(_ context.Context, _ *mcp.CallToolRequest, a messageArgs) (*mcp.CallToolResult, any, error) {
			conversation, err := s.find(a.SessionID)
			if err != nil {
				return nil, nil, err
			}
			if strings.TrimSpace(a.Message) == "" {
				return nil, nil, errors.New("message is required")
			}
			if err := s.steer(conversation, a.Message, map[string]any{"sessionUpdate": steered, "message": a.Message}); err != nil {
				return nil, nil, err
			}
			return said("Devin has the message and is going on with it; its reply to the subagent call covers what it does now.")
		})
	mcp.AddTool(server, &mcp.Tool{Name: "stop_task", Title: "Stop a Devin task",
		Description: "Tell a running Devin task to stop now and report what it did, what is left, and the state it leaves things in; its turn is cancelled if it has not finished within timeout_seconds. Its report is its reply to the subagent call. Unlike cancelling the subagent, the work done so far is reported, not lost."},
		func(_ context.Context, _ *mcp.CallToolRequest, a stopArgs) (*mcp.CallToolResult, any, error) {
			conversation, err := s.find(a.SessionID)
			if err != nil {
				return nil, nil, err
			}
			if err := s.steer(conversation, stopMessage, map[string]any{"sessionUpdate": stopAsked}); err != nil {
				return nil, nil, err
			}
			timeout := a.timeout()
			s.mu.Lock()
			ended, r := conversation.turnEnded, conversation.r
			s.mu.Unlock()
			go func() {
				select {
				case <-ended:
				case <-time.After(time.Duration(timeout * float64(time.Second))):
					log.Printf("%s did not stop within %vs; cancelling its turn", r.devinSession, timeout)
					s.d.notifyDevin("session/cancel", map[string]any{"sessionId": r.devinSession})
				}
			}()
			return said("Devin is told to stop and report; its report is its reply to the subagent call. Its turn is cancelled if it has not finished in " + strconv.Itoa(int(timeout)) + "s.")
		})
	return server
}

// steer hands Devin a message in the middle of a conversation's turn, and the turn's watcher the
// update that says so.
func (s *server) steer(conversation *served, message string, said map[string]any) error {
	s.mu.Lock()
	live, session := conversation.live, conversation.session
	s.mu.Unlock()
	if !live {
		return fmt.Errorf("Devin is not working on session %s now; continue it with the subagent tool and this sessionID", session)
	}
	r := conversation.r
	r.mu.Lock()
	watch := r.watch
	r.mu.Unlock()
	if watch != nil {
		watch(rootAgent, said)
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
	return nil
}

// limit is the rate limit too long to wait out within a turn that Devin is under, if any.
func (s *server) limit(w http.ResponseWriter, _ *http.Request) {
	state := map[string]any{"limited": false}
	if l, limited := s.d.limitedUntil(time.Now()); limited {
		state = map[string]any{"limited": true, "until": l.until, "message": l.message}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.tasks(24 * time.Hour))
}

// statusOf is one task and its lines from ?since= on (the latest ?lines= of them, 40 by default).
func (s *server) statusOf(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.find(r.PathValue("session"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	since, sinceGiven := -1, r.URL.Query().Has("since")
	if sinceGiven {
		since, _ = strconv.Atoi(r.URL.Query().Get("since"))
	}
	n, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || n <= 0 {
		n = 40
	}
	lines, next := conversation.act.since(max(since, 0), false)
	if !sinceGiven && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s.mu.Lock()
	t := s.taskOf(conversation)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"task": t, "lines": lines, "next": next})
}
