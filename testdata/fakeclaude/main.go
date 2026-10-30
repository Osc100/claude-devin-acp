// Command fakeclaude stands in for the Claude adapter in the end-to-end test: an ACP agent that
// answers every prompt by calling the `devin` tool of the MCP server it was handed, with the
// prompt's text as the task, and saying what the tool returned. No model is asked anything.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	out := json.NewEncoder(os.Stdout)
	var devinURL string
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var msg map[string]any
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg["method"] == nil {
			continue
		}
		params, _ := msg["params"].(map[string]any)
		reply := map[string]any{"jsonrpc": "2.0", "id": msg["id"]}
		switch msg["method"] {
		case "initialize":
			reply["result"] = map[string]any{"protocolVersion": params["protocolVersion"],
				"agentCapabilities": map[string]any{"mcpCapabilities": map[string]any{"http": true}}}
		case "session/new":
			for _, s := range params["mcpServers"].([]any) {
				if server := s.(map[string]any); server["name"] == "devin" {
					devinURL = server["url"].(string)
				}
			}
			reply["result"] = map[string]any{"sessionId": "claude-1"}
		case "session/prompt":
			task := params["prompt"].([]any)[0].(map[string]any)["text"].(string)
			text := callDevin(devinURL, task)
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "claude-1",
				"update": map[string]any{"sessionUpdate": "agent_message_chunk",
					"content": map[string]any{"type": "text", "text": text}},
			}})
			reply["result"] = map[string]any{"stopReason": "end_turn"}
		default:
			if msg["id"] == nil {
				continue
			}
			reply["error"] = map[string]any{"code": -32601, "message": "fakeclaude does not do that"}
		}
		_ = out.Encode(reply)
	}
}

func callDevin(url, task string) string {
	if url == "" {
		return "no devin MCP server was handed over"
	}
	post := func(body map[string]any) map[string]any {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		defer resp.Body.Close()
		var reply map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		return reply
	}
	post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
	listed := post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	start := time.Now()
	started := post(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "devin", "arguments": map[string]any{"task": task, "title": "e2e"}}})
	startedIn := time.Since(start)
	waited := post(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "devin_wait", "arguments": map[string]any{"timeout_seconds": 300}}})
	raw, _ := json.Marshal(map[string]any{"listed": listed["result"] != nil, "started": started,
		"startedInSeconds": startedIn.Seconds(), "waited": waited})
	return fmt.Sprintf("%s", raw)
}
