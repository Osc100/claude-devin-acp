package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd drives the built proxy as an editor would, with testdata/fakeclaude as Claude and
// the real Devin CLI behind it. It spends Devin quota, so it runs only with CLAUDE_DEVIN_E2E=1;
// CLAUDE_DEVIN_E2E_DIR is the directory Devin works in (a scratch repo with calc.py and main.py).
func TestEndToEnd(t *testing.T) {
	if os.Getenv("CLAUDE_DEVIN_E2E") != "1" {
		t.Skip("spends Devin quota: set CLAUDE_DEVIN_E2E=1")
	}
	dir := t.TempDir()
	for name, pkg := range map[string]string{"proxy": ".", "fakeclaude": "./testdata/fakeclaude"} {
		if out, err := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg).CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", name, err, out)
		}
	}
	work := os.Getenv("CLAUDE_DEVIN_E2E_DIR")
	if work == "" {
		t.Fatal("set CLAUDE_DEVIN_E2E_DIR")
	}
	proxy := exec.Command(filepath.Join(dir, "proxy"), "--claude", filepath.Join(dir, "fakeclaude"))
	proxy.Stderr = os.Stderr
	in, _ := proxy.StdinPipe()
	out, _ := proxy.StdoutPipe()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer proxy.Process.Kill()

	send := func(msg map[string]any) {
		line, _ := json.Marshal(msg)
		_, _ = in.Write(append(line, '\n'))
	}
	lines := make(chan map[string]any, 1024)
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				lines <- msg
			}
		}
		close(lines)
	}()
	var seen []map[string]any
	await := func(id float64) map[string]any {
		deadline := time.After(5 * time.Minute)
		for {
			select {
			case msg, ok := <-lines:
				if !ok {
					t.Fatal("the proxy closed its output")
				}
				seen = append(seen, msg)
				if msg["method"] == "session/request_permission" {
					send(map[string]any{"jsonrpc": "2.0", "id": msg["id"],
						"result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}})
				}
				if got, _ := msg["id"].(float64); got == id && msg["method"] == nil {
					return msg
				}
			case <-deadline:
				t.Fatalf("no answer to request %v", id)
			}
		}
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": 2, "clientCapabilities": map[string]any{"subagents": map[string]any{}}}})
	if v := await(1)["result"].(map[string]any)["protocolVersion"]; v != float64(1) {
		t.Errorf("an editor asking for v2 was answered %v, not 1", v)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new",
		"params": map[string]any{"cwd": work, "mcpServers": []any{}}})
	await(2)
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{
		"sessionId": "claude-1", "prompt": []any{map[string]any{"type": "text", "text": "Spawn exactly one " +
			"`subagent_explore` subagent (this test chooses that profile explicitly; `swe2-high` does not exist) " +
			"to read calc.py and main.py and report what main.py prints, wait for it, then answer in one sentence. Edit nothing."}}}})
	await(3)

	var runAnnounced, subAnnounced, subEnded, runEnded bool
	var answer string
	for _, msg := range seen {
		if msg["method"] != "session/update" {
			continue
		}
		session, update := updateOf(msg)
		child, _ := update["sessionId"].(string)
		switch update["sessionUpdate"] {
		case "subagent_update":
			state, _ := update["state"].(map[string]any)
			running := state["state"] == "running"
			finished := state["state"] == "idle" && state["stopReason"] == "end_turn"
			switch {
			case session == "claude-1" && strings.HasPrefix(child, "devin:"):
				runAnnounced = runAnnounced || running
				runEnded = runEnded || finished
			case strings.HasPrefix(session, "devin:") && strings.Contains(child, "/"):
				subAnnounced = subAnnounced || running
				subEnded = subEnded || finished
			}
		case "agent_message_chunk":
			if session == "claude-1" {
				answer += update["content"].(map[string]any)["text"].(string)
			}
		}
	}
	t.Logf("Claude was told: %s", answer)
	if !runAnnounced || !runEnded {
		t.Errorf("the Devin run: announced %v, ended %v", runAnnounced, runEnded)
	}
	if !subAnnounced || !subEnded {
		t.Errorf("Devin's subagent: announced %v, ended %v", subAnnounced, subEnded)
	}
	var said struct {
		StartedInSeconds float64 `json:"startedInSeconds"`
	}
	_ = json.Unmarshal([]byte(answer), &said)
	if said.StartedInSeconds > 10 {
		t.Errorf("devin took %.1fs to answer: it must return as the run starts, not when it ends", said.StartedInSeconds)
	}
	if !strings.Contains(answer, "in the background as run devin:") {
		t.Errorf("devin did not say which run it started")
	}
	if !strings.Contains(answer, "finished") || !strings.Contains(answer, "5") {
		t.Errorf("devin_wait did not hand back Devin's answer")
	}
}

// TestSoloEndToEnd is Devin + Devin against the real Devin CLI: the test is the editor, the proxy
// runs with --devin-only. Opt-in for the same reason as TestEndToEnd.
func TestSoloEndToEnd(t *testing.T) {
	if os.Getenv("CLAUDE_DEVIN_E2E") != "1" {
		t.Skip("spends Devin quota: set CLAUDE_DEVIN_E2E=1")
	}
	work := os.Getenv("CLAUDE_DEVIN_E2E_DIR")
	if work == "" {
		t.Fatal("set CLAUDE_DEVIN_E2E_DIR")
	}
	bin := filepath.Join(t.TempDir(), "proxy")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building: %v\n%s", err, out)
	}
	proxy := exec.Command(bin, "--devin-only")
	proxy.Stderr = os.Stderr
	in, _ := proxy.StdinPipe()
	out, _ := proxy.StdoutPipe()
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer proxy.Process.Kill()
	send := func(msg map[string]any) {
		line, _ := json.Marshal(msg)
		_, _ = in.Write(append(line, '\n'))
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var seen []map[string]any
	await := func(id float64) map[string]any {
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) != nil {
				continue
			}
			seen = append(seen, msg)
			if msg["method"] == "session/request_permission" {
				send(map[string]any{"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}})
			}
			if got, _ := msg["id"].(float64); got == id && msg["method"] == nil {
				return msg
			}
		}
		t.Fatalf("the proxy closed before answering %v", id)
		return nil
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{"subagents": map[string]any{}}}})
	await(1)
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new",
		"params": map[string]any{"cwd": work, "mcpServers": []any{}}})
	root := await(2)["result"].(map[string]any)["sessionId"].(string)
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{
		"sessionId": root, "prompt": []any{map[string]any{"type": "text", "text": "Use exactly one subagent " +
			"(spawn it with your subagent tool) to read calc.py and main.py and report what main.py prints, " +
			"wait for it, then answer in one sentence. Do not edit any file."}}}})
	if stop := await(3)["result"].(map[string]any)["stopReason"]; stop != "end_turn" {
		t.Errorf("Devin stopped with %v", stop)
	}
	var announced, ended bool
	var subCalls int
	for _, msg := range seen {
		if msg["method"] != "session/update" {
			continue
		}
		session, update := updateOf(msg)
		child, _ := update["sessionId"].(string)
		switch {
		case update["sessionUpdate"] == "subagent_update" && session == root && strings.HasPrefix(child, root+"/"):
			state, _ := update["state"].(map[string]any)
			announced = announced || state["state"] == "running"
			ended = ended || (state["state"] == "idle" && state["stopReason"] == "end_turn")
			if desc, _ := update["description"].(string); desc != "" {
				t.Logf("subagent %s: %s", child, desc)
			}
		case update["sessionUpdate"] == "tool_call" && strings.HasPrefix(session, root+"/"):
			subCalls++
		}
	}
	if !announced || !ended {
		t.Errorf("Devin's subagent: announced %v, ended %v", announced, ended)
	}
	if subCalls == 0 {
		t.Error("the subagent's own calls never reached its thread")
	}
}
