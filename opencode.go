package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenCode's own subagents, which run any model but Devin (DeepSeek, say, while Devin is rate
// limited): the same watching and steering as Devin's tasks get at /mcp, through OpenCode's server
// API instead of Devin's, at /mcp/subagents. OpenCode's server is found as its TUI finds it, in
// $XDG_STATE_HOME/opencode/service.json, read again on every call since a restart of OpenCode
// changes its address and password.

// opencodeCall makes one request of the OpenCode server and decodes its answer's `data`.
func opencodeCall(method, path string, body any, into any) error {
	state, _ := os.UserHomeDir()
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		state = dir
	} else {
		state = filepath.Join(state, ".local", "state")
	}
	raw, err := os.ReadFile(filepath.Join(state, "opencode", "service.json"))
	if err != nil {
		return fmt.Errorf("no OpenCode server found: %w", err)
	}
	var service struct {
		URL      string `json:"url"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &service); err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, service.URL+path, payload)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:"+service.Password)))
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if response.StatusCode >= 300 {
		return fmt.Errorf("OpenCode answered %s: %.300s", response.Status, answer)
	}
	if into == nil || len(answer) == 0 {
		return nil
	}
	var wrapped struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(answer, &wrapped); err != nil {
		return err
	}
	return json.Unmarshal(wrapped.Data, into)
}

// compactSession asks OpenCode to compact a session, at its next step boundary.
func compactSession(session string) error {
	if session == "" {
		return fmt.Errorf("OPENCODE_SESSION_ID is not set: not run by OpenCode")
	}
	return opencodeCall(http.MethodPost, "/api/session/"+url.PathEscape(session)+"/compact", map[string]any{}, nil)
}

// ocSession is a session as OpenCode's API gives it.
type ocSession struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Title    string `json:"title"`
	Agent    string `json:"agent"`
	Model    struct {
		ID       string `json:"id"`
		Provider string `json:"providerID"`
		Variant  string `json:"variant"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

func (s ocSession) model() string {
	name := s.Model.Provider + "/" + s.Model.ID
	if s.Model.Variant != "" && s.Model.Variant != "default" {
		name += "#" + s.Model.Variant
	}
	return name
}

// subagent is whether a session is one of OpenCode's own subagents: a child session whose model
// is not Devin, whose subagents /mcp watches.
func (s ocSession) subagent() bool {
	return s.ParentID != "" && s.Model.Provider != "devin"
}

func activeSessions() (map[string]bool, error) {
	var active map[string]struct {
		Type string `json:"type"`
	}
	if err := opencodeCall(http.MethodGet, "/api/session/active", nil, &active); err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for id, state := range active {
		running[id] = state.Type == "running"
	}
	return running, nil
}

func getSession(id string) (ocSession, error) {
	var s ocSession
	err := opencodeCall(http.MethodGet, "/api/session/"+url.PathEscape(id), nil, &s)
	return s, err
}

// ocMessage is a message of a session as OpenCode's API gives it.
type ocMessage struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Content []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name"`
		State struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
		} `json:"state"`
	} `json:"content"`
}

// sessionLines is what a session's latest messages said and did, oldest first, as lines: its text,
// a line for each tool call, and the messages it was sent.
func sessionLines(id string, messages int) ([]string, error) {
	var latest []ocMessage
	if err := opencodeCall(http.MethodGet, "/api/session/"+url.PathEscape(id)+"/message?limit="+strconv.Itoa(messages), nil, &latest); err != nil {
		return nil, err
	}
	var lines []string
	for i := len(latest) - 1; i >= 0; i-- {
		m := latest[i]
		switch m.Type {
		case "user", "synthetic":
			if text := strings.TrimSpace(m.Text); text != "" {
				lines = append(lines, iconMessage+" "+clip(text, 200))
			}
		case "assistant":
			for _, part := range m.Content {
				switch part.Type {
				case "text":
					for _, line := range strings.Split(part.Text, "\n") {
						if strings.TrimSpace(line) != "" {
							lines = append(lines, line)
						}
					}
				case "tool":
					line := iconTool + " " + part.Name
					for _, key := range []string{"command", "path", "filePath", "pattern", "query", "url", "description"} {
						if value, _ := part.State.Input[key].(string); value != "" {
							line += ": " + clip(value, 160)
							break
						}
					}
					switch part.State.Status {
					case "running":
						line += " (running)"
					case "error":
						line = iconError + " " + strings.TrimPrefix(line, iconTool+" ") + " failed"
					}
					lines = append(lines, line)
				}
			}
		}
	}
	return lines, nil
}

const subagentTools = `[{
  "name": "task_status",
  "title": "List OpenCode subagents",
  "description": "List OpenCode's own subagents (any model but Devin, such as DeepSeek) running now and those finished in the last hour: each one's sessionID, title, model, how long it has been working (or since it finished), and the last thing it did. A quick check that needs no waiting; use peek_task for more of one.",
  "inputSchema": {"type": "object", "properties": {}}
}, {
  "name": "peek_task",
  "title": "Peek at an OpenCode subagent",
  "description": "Return the latest of what an OpenCode subagent has said and done, running or finished: its text, a line for each tool call, and the messages it was sent, oldest first. To see whether it is on track before steering it with message_task.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "sessionID": {"type": "string", "description": "The subagent's session, as the subagent tool gave it."},
      "lines": {"type": "number", "description": "How many of the latest lines to return. Default 40, at most 200."}
    },
    "required": ["sessionID"]
  }
}, {
  "name": "message_task",
  "title": "Message a running OpenCode subagent",
  "description": "Send a message to an OpenCode subagent while it works, without waiting for it to finish: to correct it, narrow or widen the task, add what you have learned, or tell it to stop and report. It reads the message at its next step, once its current tool call returns; interrupt cuts that call short (a long build or bench) so it reads the message at once. Only for a subagent still running; a finished one is continued with the subagent tool and its sessionID.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "sessionID": {"type": "string", "description": "The subagent's session."},
      "message": {"type": "string", "description": "What to tell it."},
      "interrupt": {"type": "boolean", "description": "Cut its current tool call short so it reads the message at once. Default false."}
    },
    "required": ["sessionID", "message"]
  }
}, {
  "name": "stop_task",
  "title": "Stop an OpenCode subagent",
  "description": "Tell a running OpenCode subagent to stop now and report what it did, what is left, and the state it leaves things in; it is interrupted if it has not finished within timeout_seconds. Its report is its reply to the subagent call.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "sessionID": {"type": "string", "description": "The subagent's session."},
      "timeout_seconds": {"type": "number", "description": "How long it has to wrap up before it is interrupted. Default 60."}
    },
    "required": ["sessionID"]
  }
}]`

func serveSubagents(w http.ResponseWriter, r *http.Request) {
	serveMCP(w, r, subagentTools, callSubagents)
}

func callSubagents(params map[string]any) map[string]any {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if name == "task_status" {
		text, err := subagentStatus()
		if err != nil {
			return toolResult(err.Error(), true)
		}
		return toolResult(text, false)
	}
	id, _ := args["sessionID"].(string)
	session, err := getSession(id)
	if err != nil {
		return toolResult("no OpenCode session "+id+": "+err.Error(), true)
	}
	if !session.subagent() {
		return toolResult("session "+id+" is not one of OpenCode's own subagents (a Devin subagent is watched through the devin MCP server)", true)
	}
	running, err := activeSessions()
	if err != nil {
		return toolResult(err.Error(), true)
	}
	switch name {
	case "peek_task":
		n := 40
		if lines, ok := args["lines"].(float64); ok && lines > 0 {
			n = min(int(lines), 200)
		}
		lines, err := sessionLines(id, 30)
		if err != nil {
			return toolResult(err.Error(), true)
		}
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		return toolResult(describeSubagent(session, running[id], "")+"\n\n"+strings.Join(lines, "\n"), false)
	case "message_task":
		message, _ := args["message"].(string)
		if strings.TrimSpace(message) == "" {
			return toolResult("message is required", true)
		}
		if !running[id] {
			return toolResult("session "+id+" is not running now; continue it with the subagent tool and this sessionID", true)
		}
		interrupt, _ := args["interrupt"].(bool)
		if err := steerSubagent(id, message, interrupt); err != nil {
			return toolResult(err.Error(), true)
		}
		log.Printf("steered OpenCode subagent %s (interrupt %v)", id, interrupt)
		return toolResult("The subagent has the message and reads it at its next step; its reply to the subagent call covers what it does then.", false)
	case "stop_task":
		if !running[id] {
			return toolResult("session "+id+" is not running now", true)
		}
		timeout := 60.0
		if seconds, ok := args["timeout_seconds"].(float64); ok && seconds > 0 {
			timeout = seconds
		}
		if err := steerSubagent(id, stopMessage, false); err != nil {
			return toolResult(err.Error(), true)
		}
		go func() {
			deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
			for time.Now().Before(deadline) {
				time.Sleep(5 * time.Second)
				if running, err := activeSessions(); err == nil && !running[id] {
					return
				}
			}
			log.Printf("OpenCode subagent %s did not stop within %vs; interrupting it", id, timeout)
			_ = opencodeCall(http.MethodPost, "/api/session/"+url.PathEscape(id)+"/interrupt", nil, nil)
		}()
		return toolResult("The subagent is told to stop and report; its report is its reply to the subagent call. It is interrupted if it has not finished in "+strconv.Itoa(int(timeout))+"s.", false)
	}
	return toolResult("no tool "+name, true)
}

// steerSubagent hands a running OpenCode session a message, delivered at its next step, and when
// interrupt is set cuts its current step short so that comes at once.
func steerSubagent(id, message string, interrupt bool) error {
	path := "/api/session/" + url.PathEscape(id)
	if err := opencodeCall(http.MethodPost, path+"/prompt", map[string]any{"text": message, "delivery": "steer"}, nil); err != nil {
		return err
	}
	if interrupt {
		// resume is a query parameter: the steered message is taken up once the step is cut.
		return opencodeCall(http.MethodPost, path+"/interrupt?resume=true", nil, nil)
	}
	return nil
}

// clip is a value's first line, cut to n runes.
func clip(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return s
}

func describeSubagent(s ocSession, working bool, last string) string {
	state := "finished " + ago(time.UnixMilli(s.Time.Updated)) + " ago"
	if working {
		state = "working for " + ago(time.UnixMilli(s.Time.Created))
	}
	line := fmt.Sprintf("%s (%s, %s): %s\n  %s, in %s", s.ID, s.Agent, s.model(), s.Title, state, s.Location.Directory)
	if last != "" {
		line += "\n  last: " + last
	}
	return line
}

// subagentStatus lists OpenCode's own subagents running now or finished within the hour.
func subagentStatus() (string, error) {
	running, err := activeSessions()
	if err != nil {
		return "", err
	}
	var sessions []ocSession
	if err := opencodeCall(http.MethodGet, "/api/session?limit=100", nil, &sessions); err != nil {
		return "", err
	}
	var shown []ocSession
	for _, s := range sessions {
		recent := time.Since(time.UnixMilli(s.Time.Updated)) < time.Hour
		if s.subagent() && (running[s.ID] || recent) {
			shown = append(shown, s)
		}
	}
	if len(shown) == 0 {
		return "no OpenCode subagents running or finished in the last hour", nil
	}
	sort.SliceStable(shown, func(i, j int) bool { return running[shown[i].ID] && !running[shown[j].ID] })
	described := make([]string, len(shown))
	for i, s := range shown {
		last := ""
		if lines, err := sessionLines(s.ID, 1); err == nil && len(lines) > 0 {
			last = lines[len(lines)-1]
		}
		described[i] = describeSubagent(s, running[s.ID], last)
	}
	return strings.Join(described, "\n\n"), nil
}
