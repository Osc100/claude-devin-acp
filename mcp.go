package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// mcpServer is the MCP server Claude is handed in every session the editor opens, at
// `/mcp/<token>`: the token says which of Claude's sessions a call comes from, so its Devin runs
// are announced under that session. Streamable HTTP's plain form: a request is POSTed and answered
// with one JSON body. Nothing is streamed: a run is started at once and collected with
// `wait_for_tasks`, so no call has anything to say before its one answer.
type mcpServer struct{ p *proxy }

const devinTools = `[{
  "name": "start_task",
  "title": "Start a Devin task",
  "description": "Start Devin, a separate coding agent on this machine, on a task in the background, and return its run id at once. Devin works with its own tools and may start subagents of its own; the editor shows its work as a subagent of this session. Give it a complete, self-contained task, keep working, and collect its answer with wait_for_tasks. To follow up on a finished run, pass its run id as continue.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "task": {"type": "string", "description": "What Devin should do, self-contained."},
      "title": {"type": "string", "description": "A short label for the run, shown in the editor."},
      "cwd": {"type": "string", "description": "Absolute directory Devin works in. Defaults to the session's directory."},
      "continue": {"type": "string", "description": "The id of a finished run, to give that Devin session a follow-up instead of starting a new one."},
      "effort": {"type": "string", "enum": ["medium", "high", "max"], "description": "How hard Devin (SWE-2) thinks. Leave out for the default; max only for hard problems, since it is slower."}
    },
    "required": ["task"]
  }
}, {
  "name": "wait_for_tasks",
  "title": "Wait for Devin tasks",
  "description": "Wait for Devin runs to finish and return each one's status and, once finished, its answer. Waits for every named run (all of this session's runs if none are named) up to timeout_seconds; 0 only reports where they are.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "runs": {"type": "array", "items": {"type": "string"}, "description": "Run ids to wait for; all of this session's runs if left out."},
      "timeout_seconds": {"type": "number", "description": "Longest to wait, in seconds. Default 600; 0 to only check."}
    }
  }
}, {
  "name": "cancel_task",
  "title": "Cancel a Devin task",
  "description": "Stop a Devin run's current turn.",
  "inputSchema": {
    "type": "object",
    "properties": {"run": {"type": "string", "description": "The run id to stop."}},
    "required": ["run"]
  }
}]`

func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, found := strings.CutPrefix(r.URL.Path, "/mcp/")
	parent, known := s.p.sessionOf(token)
	if !found || !known {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		// No server-sent stream: there is nothing to say unasked.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "one JSON-RPC message per request", http.StatusBadRequest)
		return
	}
	id, isRequest := req["id"]
	if !isRequest {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		version, _ := params["protocolVersion"].(string)
		if version == "" {
			version = "2025-06-18"
		}
		reply["result"] = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "devin", "version": "0.1.0"},
		}
	case "ping":
		reply["result"] = map[string]any{}
	case "tools/list":
		reply["result"] = map[string]any{"tools": json.RawMessage(devinTools)}
	case "tools/call":
		// Looked up at the call, not at the connection: the session was still being opened then.
		parent, _ = s.p.sessionOf(token)
		reply["result"] = s.call(r, parent, params)
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "no method " + method}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (s *mcpServer) call(r *http.Request, parent string, params map[string]any) map[string]any {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if parent == "" {
		return toolResult("the session is still being opened; call "+name+" again", true)
	}
	switch name {
	case "start_task":
		task, _ := args["task"].(string)
		if strings.TrimSpace(task) == "" {
			return toolResult("task is required", true)
		}
		title, _ := args["title"].(string)
		cwd, _ := args["cwd"].(string)
		cont, _ := args["continue"].(string)
		effort, _ := args["effort"].(string)
		run, err := s.p.devin.begin(parent, task, title, cwd, cont, effort)
		if err != nil {
			log.Printf("starting a Devin run for %s: %v", parent, err)
			return toolResult("Devin did not start: "+err.Error(), true)
		}
		return toolResult("Devin is working on it in the background as run "+run.child+
			". Keep working; collect its answer with wait_for_tasks.", false)
	case "wait_for_tasks":
		var ids []string
		if listed, ok := args["runs"].([]any); ok {
			for _, id := range listed {
				if id, ok := id.(string); ok {
					ids = append(ids, id)
				}
			}
		}
		timeout := 600.0
		if seconds, ok := args["timeout_seconds"].(float64); ok && seconds >= 0 {
			timeout = seconds
		}
		runs, err := s.p.runsOf(parent, ids)
		if err != nil {
			return toolResult(err.Error(), true)
		}
		if len(runs) == 0 {
			return toolResult("no Devin runs in this session", false)
		}
		wait(r.Context(), runs, time.Duration(timeout*float64(time.Second)))
		reports := make([]string, len(runs))
		for i, run := range runs {
			reports[i] = run.report()
		}
		return toolResult(strings.Join(reports, "\n\n"), false)
	case "cancel_task":
		id, _ := args["run"].(string)
		runs, err := s.p.runsOf(parent, []string{id})
		if err != nil {
			return toolResult(err.Error(), true)
		}
		s.p.cancelChild(runs[0].child)
		return toolResult("asked Devin to stop run "+id+"; wait_for_tasks reports when it has", false)
	}
	return toolResult("no tool "+name, true)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}
