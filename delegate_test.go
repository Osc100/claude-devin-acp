package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func openedSession(t *testing.T, devinOnly bool, params map[string]any) map[string]any {
	t.Helper()
	var claude bytes.Buffer
	p := &proxy{claude: &wire{w: &claude}, mcpBase: "http://127.0.0.1:1/mcp/", devinOnlyDelegate: devinOnly,
		tokens: map[string]string{}, cwds: map[string]string{}, opening: map[string]opening{}}
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/new", "params": params})
	p.fromEditor(line)
	var sent map[string]any
	if err := json.Unmarshal(claude.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	return sent["params"].(map[string]any)
}

func TestClaudeIsLeftDevinAloneToDelegateWith(t *testing.T) {
	params := openedSession(t, true, map[string]any{"cwd": "/w", "mcpServers": []any{}, "_meta": map[string]any{
		"claudeCode":   map[string]any{"options": map[string]any{"disallowedTools": []any{"WebSearch"}}},
		"systemPrompt": map[string]any{"append": "The editor's own note."},
	}})
	meta := params["_meta"].(map[string]any)
	disallowed := meta["claudeCode"].(map[string]any)["options"].(map[string]any)["disallowedTools"].([]any)
	got := map[string]bool{}
	for _, tool := range disallowed {
		got[tool.(string)] = true
	}
	if !got["Agent"] || !got["Task"] || !got["WebSearch"] {
		t.Errorf("disallowed %v: Claude's subagent tools must go, and the editor's own must stay", disallowed)
	}
	appended := meta["systemPrompt"].(map[string]any)["append"].(string)
	if !strings.HasPrefix(appended, "The editor's own note.") || !strings.Contains(appended, "mcp__devin__start_task") {
		t.Errorf("the system prompt's addition is %q", appended)
	}
	if servers := params["mcpServers"].([]any); len(servers) != 1 {
		t.Errorf("%d MCP servers; the devin one is added", len(servers))
	}
}

func TestWithClaudeSubagentsTheSessionIsLeftAlone(t *testing.T) {
	params := openedSession(t, false, map[string]any{"cwd": "/w", "mcpServers": []any{}})
	if _, touched := params["_meta"]; touched {
		t.Errorf("_meta was set: %v", params["_meta"])
	}
}

// Claude connects to its MCP servers while it opens a session, before its answer names the
// session: the devin server must answer then, or Claude reports it failed to start.
func TestTheDevinServerAnswersBeforeTheSessionIsOpen(t *testing.T) {
	var claude bytes.Buffer
	p := &proxy{claude: &wire{w: &claude}, mcpBase: "http://127.0.0.1:1/mcp/",
		tokens: map[string]string{}, cwds: map[string]string{}, opening: map[string]opening{}}
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/new",
		"params": map[string]any{"cwd": "/w", "mcpServers": []any{}}})
	p.fromEditor(line)
	var sent map[string]any
	_ = json.Unmarshal(claude.Bytes(), &sent)
	server := sent["params"].(map[string]any)["mcpServers"].([]any)[0].(map[string]any)
	path := strings.TrimPrefix(server["url"].(string), "http://127.0.0.1:1")
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	recorder := httptest.NewRecorder()
	(&mcpServer{p: p}).ServeHTTP(recorder, httptest.NewRequest("POST", path, body))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"start_task"`) {
		t.Errorf("before Claude answered session/new, the server said %d: %s", recorder.Code, recorder.Body)
	}
}
