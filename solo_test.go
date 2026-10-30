package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func soloProxy() (*devin, *bytes.Buffer, *bytes.Buffer) {
	var editor, devinIn bytes.Buffer
	p := &proxy{editor: &wire{w: &editor}, children: map[string]*run{}, waiting: map[string]chan map[string]any{}}
	d := newDevin(nil, p)
	p.devin = d
	d.in = &wire{w: &devinIn}
	return d, &editor, &devinIn
}

func lines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
	}
	return out
}

func TestDevinIsToldToReportSubagentsOnlyByAnEditorThatShowsThem(t *testing.T) {
	for _, shows := range []bool{true, false} {
		d, _, devinIn := soloProxy()
		caps := map[string]any{"_meta": map[string]any{"zed.dev/other": true}}
		if shows {
			caps["subagents"] = map[string]any{}
		}
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "initialize",
			"params": map[string]any{"protocolVersion": 2, "clientCapabilities": caps}})
		d.soloFromEditor(line)
		params := lines(t, devinIn)[0]["params"].(map[string]any)
		meta := params["clientCapabilities"].(map[string]any)["_meta"].(map[string]any)
		if (meta["cognition.ai/subagentSupport"] == true) != shows || (meta["cognition.ai/subagentControl"] == true) != shows {
			t.Errorf("editor shows subagents %v: Devin was sent %v", shows, meta)
		}
		if meta["zed.dev/other"] != true {
			t.Error("the editor's own _meta was lost")
		}
		if params["protocolVersion"] != float64(1) {
			t.Errorf("Devin was asked for v%v", params["protocolVersion"])
		}
	}
}

func TestDevinsOwnThreadPassesThroughAndItsSubagentBecomesAChild(t *testing.T) {
	d, editor, _ := soloProxy()
	d.p.subagents = true
	recorded, err := os.Open("testdata/devin-subagent.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer recorded.Close()
	scanner := bufio.NewScanner(recorded)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	in := 0
	for scanner.Scan() {
		d.soloFromDevin(append([]byte(nil), scanner.Bytes()...))
		in++
	}
	const root, sub = "nettle-marimba", "nettle-marimba/69fb0515"
	var announced, ended, rootUsage bool
	var subCalls, out int
	for _, msg := range lines(t, editor) {
		out++
		if msg["method"] != "session/update" {
			continue
		}
		session, update := updateOf(msg)
		kind := update["sessionUpdate"]
		switch {
		case kind == "subagent_update" && update["sessionId"] == sub && session == root:
			state := update["state"].(map[string]any)
			announced = announced || state["state"] == "running"
			ended = ended || (state["state"] == "idle" && state["stopReason"] == "end_turn")
		case kind == "tool_call" && session == sub:
			subCalls++
		case kind == "usage_update" && session == root:
			rootUsage = true
		}
		if session == root && kind == "tool_call" {
			if meta, _ := update["_meta"].(map[string]any); ownerOf(meta) != rootAgent {
				t.Errorf("the subagent's call stayed in Devin's own thread: %v", update)
			}
		}
	}
	if !announced || !ended {
		t.Errorf("the subagent: announced %v, ended %v", announced, ended)
	}
	if subCalls != 2 {
		t.Errorf("%d tool calls in the subagent's thread; it read two files", subCalls)
	}
	if !rootUsage {
		t.Error("Devin's own thread lost its usage updates, which pass through untouched")
	}
	t.Logf("%d lines from Devin, %d to the editor", in, out)
}
