package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// replay feeds a recorded Devin session — CLI 3000.11.234 starting one explore subagent that reads
// two files — through the translation, and returns what the editor was sent.
func replay(t *testing.T, shown bool) (*run, []map[string]any) {
	t.Helper()
	var editor bytes.Buffer
	p := &proxy{editor: &wire{w: &editor}, children: map[string]*run{}, waiting: map[string]chan map[string]any{}}
	d := newDevin(nil, p)
	p.devin = d
	r := &run{parent: "claude-1", child: "devin:nettle-marimba", devinSession: "nettle-marimba", shown: shown,
		agents: map[string]string{rootAgent: "devin:nettle-marimba"}}
	d.runs["nettle-marimba"] = r

	recorded, err := os.Open("testdata/devin-subagent.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer recorded.Close()
	scanner := bufio.NewScanner(recorded)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		if msg["method"] == "session/update" {
			d.update(msg["params"].(map[string]any))
		}
	}
	var sent []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(editor.String()), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, msg)
	}
	return r, sent
}

func updateOf(msg map[string]any) (string, map[string]any) {
	params := msg["params"].(map[string]any)
	return params["sessionId"].(string), params["update"].(map[string]any)
}

func TestADevinSubagentBecomesAChildOfTheRun(t *testing.T) {
	r, sent := replay(t, true)
	const sub = "devin:nettle-marimba/69fb0515"
	var announced, ended bool
	var messages, subCalls, rootCalls int
	for _, msg := range sent {
		session, update := updateOf(msg)
		kind := update["sessionUpdate"]
		switch {
		case kind == "subagent_update" && update["sessionId"] == sub:
			if session != r.child {
				t.Errorf("the subagent was announced under %s, not its run %s", session, r.child)
			}
			state := update["state"].(map[string]any)
			if state["state"] == "running" {
				if ended {
					t.Error("the subagent was announced after it ended")
				}
				announced = true
				if update["title"] != "Read calc.py and main.py, report output" {
					t.Errorf("its title is %v", update["title"])
				}
			} else if state["state"] == "idle" && state["stopReason"] == "end_turn" {
				ended = true
			}
		case kind == "session_message":
			messages++
		case kind == "tool_call" && session == sub:
			subCalls++
		case kind == "tool_call" && session == r.child:
			rootCalls++
		}
		if id, _ := update["toolCallId"].(string); id == "69fb0515" {
			t.Errorf("Devin's marker call for its subagent reached the editor: %v", update)
		}
		if kind == "usage_update" || kind == "current_mode_update" || kind == "available_commands_update" {
			t.Errorf("Devin's own %v reached the editor", kind)
		}
	}
	if !announced || !ended {
		t.Errorf("announced %v, ended %v: the subagent needs both", announced, ended)
	}
	if messages != 2 {
		t.Errorf("%d session messages; the task is laid down once in each transcript", messages)
	}
	if subCalls != 2 {
		t.Errorf("%d tool calls in the subagent's transcript; it read two files", subCalls)
	}
	if rootCalls == 0 {
		t.Error("the run's own calls (starting and checking on the subagent) never reached its transcript")
	}
	if !strings.Contains(r.answer.String(), "\n\n") {
		t.Errorf("Devin's two messages run together: %q", r.answer.String())
	}
	if !strings.Contains(r.answer.String(), "5") {
		t.Errorf("the answer for Claude is %q, which does not say main.py prints 5", r.answer.String())
	}
}

func TestNothingIsAnnouncedToAnEditorThatShowsNoSubagents(t *testing.T) {
	r, sent := replay(t, false)
	if len(sent) != 0 {
		t.Errorf("%d messages reached an editor that did not offer subagents", len(sent))
	}
	if r.answer.Len() == 0 {
		t.Error("Claude would get no answer")
	}
}
