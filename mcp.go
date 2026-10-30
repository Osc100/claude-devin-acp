package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// mcpServer is the MCP server Claude is handed in every session the editor opens, at
// `/mcp/<token>`: the token says which of Claude's sessions a call comes from, so its Devin run
// is announced under that session. Streamable HTTP's plain form: a request is POSTed and answered
// with one JSON body; nothing is streamed, since the only tool answers once, when Devin is done.
type mcpServer struct{ p *proxy }

const devinTool = `{
  "name": "devin",
  "description": "Hand a task to Devin, a separate coding agent running on this machine, and wait for its answer. Devin works in the given directory with its own tools and may start subagents of its own; the editor shows its work as a subagent of this session. Give it a complete, self-contained task. To follow up on an earlier run, pass that run's id as continue.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "task": {"type": "string", "description": "What Devin should do, self-contained."},
      "title": {"type": "string", "description": "A short label for the run, shown in the editor."},
      "cwd": {"type": "string", "description": "Absolute directory Devin works in. Defaults to the session's directory as Devin finds it."},
      "continue": {"type": "string", "description": "The run id an earlier call returned, to give that Devin session a follow-up instead of starting a new one."}
    },
    "required": ["task"]
  }
}`

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
		reply["result"] = map[string]any{"tools": []json.RawMessage{json.RawMessage(devinTool)}}
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
	if name, _ := params["name"].(string); name != "devin" {
		return toolResult("no tool "+name, true)
	}
	if parent == "" {
		return toolResult("the session is still being opened; call devin again", true)
	}
	args, _ := params["arguments"].(map[string]any)
	task, _ := args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return toolResult("task is required", true)
	}
	title, _ := args["title"].(string)
	cwd, _ := args["cwd"].(string)
	cont, _ := args["continue"].(string)
	// The request's context ends when Claude gives the call up, which cancels Devin's turn.
	answer, runID, err := s.p.devin.do(r.Context(), parent, task, title, cwd, cont)
	if err != nil {
		log.Printf("Devin run for %s: %v", parent, err)
		text := "Devin did not finish: " + err.Error()
		if answer != "" {
			text += "\n\nWhat it said before that:\n" + answer
		}
		return toolResult(text, true)
	}
	if answer == "" {
		answer = "(Devin finished without a message.)"
	}
	return toolResult(answer+"\n\n[Devin run id: "+runID+" — pass it as continue to follow up.]", false)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}
