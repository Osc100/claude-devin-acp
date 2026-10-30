package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivityKeepsLinesAndTheOneBeingWritten(t *testing.T) {
	a := &activity{}
	a.write("first li")
	a.write("ne\n\nsecond\nthi")
	lines, next := a.since(0, false)
	if strings.Join(lines, "|") != "first line|second" || next != 2 {
		t.Errorf("got %q, next %d", lines, next)
	}
	if lines, _ := a.since(1, true); strings.Join(lines, "|") != "second|thi" {
		t.Errorf("from 1 with the partial line: %q", lines)
	}
	for range activityKept + 10 {
		a.write("x\n")
	}
	if lines, next := a.since(0, false); len(lines) != activityKept || next != activityKept+12 {
		t.Errorf("kept %d, next %d", len(lines), next)
	}
}

// Two blocks of Devin's thinking, with a tool call between, are two paragraphs, not one run-on.
func TestThoughtsAreParagraphs(t *testing.T) {
	recorder := httptest.NewRecorder()
	out := newReply(recorder, false)
	thought := func(text string) event {
		return event{rootAgent, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"text": text}}}
	}
	out.event(thought("I will look at main."))
	out.event(event{rootAgent, map[string]any{"sessionUpdate": thinkingDone}})
	out.event(thought("It is fine."))
	out.event(thought(" Done."))
	out.finish()
	var reply struct {
		Choices []struct {
			Message struct {
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &reply)
	if got := reply.Choices[0].Message.Reasoning; got != "I will look at main.\n\nIt is fine. Done." {
		t.Errorf("thoughts: %q", got)
	}
}

func TestTheSummarySaysWhatTheTurnDid(t *testing.T) {
	recorder := httptest.NewRecorder()
	out := newReply(recorder, false)
	out.event(event{rootAgent, map[string]any{"sessionUpdate": turnStopped, "cause": "complete",
		"stats": map[string]any{"filesChanged": 2.0, "commandsRun": 1.0, "toolCalls": 9.0, "modelLabel": "SWE-2 High"}}})
	out.summary(252 * time.Second)
	if got := out.content.String(); !strings.Contains(got, "2 files changed · 1 command · 9 tool calls · 4m12s · SWE-2 High") {
		t.Errorf("summary: %q", got)
	}
}

func TestTheSameCallIsDecidedAsBefore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "permissions.json")
	v := newReviewer(nil, "", "", store)
	call := map[string]any{"kind": "execute", "title": "Ran rm", "rawInput": map[string]any{"command": "rm -rf build"}}
	key, described := callKey(call, "/work")
	v.remember(key, decision{Allow: true, Reason: "build output", Call: described, Cwd: "/work"})
	again := newReviewer(nil, "", "", store)
	got := again.review(&run{cwd: "/work"}, call)
	if !got.allow || !got.remembered || got.reason != "build output" {
		t.Errorf("the kept decision was not used: %+v", got)
	}
	if other, _ := callKey(call, "/elsewhere"); other == key {
		t.Error("the same call in another directory is the same request")
	}
}

func TestAVerdictPicksItsOption(t *testing.T) {
	options := []any{
		map[string]any{"optionId": "always", "kind": "allow_always"},
		map[string]any{"optionId": "once", "kind": "allow_once"},
		map[string]any{"optionId": "no", "kind": "reject_once"},
	}
	if chosen(options, true) != "once" || chosen(options, false) != "no" {
		t.Error("wrong options")
	}
	if chosen(options[:1], true) != "always" || chosen(options[:1], false) != "" {
		t.Error("wrong fallbacks")
	}
}

// A permission request names only its call's id; what the call is comes from Devin's announcement.
func TestAPermissionRequestIsTheCallDevinAnnounced(t *testing.T) {
	r := &run{}
	r.noteCall(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "exec:0#1", "title": "Ran rm",
		"kind": "execute", "rawInput": map[string]any{"command": "rm -rf build"}})
	r.noteCall(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "exec:0#1", "status": "in_progress"})
	call := r.callOf(map[string]any{"toolCallId": "exec:0#1"})
	if call["title"] != "Ran rm" || call["rawInput"].(map[string]any)["command"] != "rm -rf build" {
		t.Errorf("the call is %v", call)
	}
	r.noteCall(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "exec:0#1", "status": "completed"})
	if len(r.calls) != 0 {
		t.Error("a finished call is kept")
	}
	if !ownTool(map[string]any{"_meta": map[string]any{"cognition.ai/permissionRule": "mcp__permissions__decide_permission"}}) ||
		ownTool(map[string]any{"_meta": map[string]any{"cognition.ai/toolName": "exec"}}) {
		t.Error("the reviewer's own tool is not told apart")
	}
}

func TestALimitIsReadOffDevinsError(t *testing.T) {
	now := time.Date(2026, 10, 30, 6, 42, 10, 0, time.UTC)
	rate := &acpError{method: "session/prompt", code: -32010, message: "Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different model. Your limit will reset in 2 minutes (at 06:44 UTC)."}
	l, ok := limitOf(fmt.Errorf("turn: %w", rate), now)
	if !ok || !l.rate || !l.until.Equal(time.Date(2026, 10, 30, 6, 44, 0, 0, time.UTC)) {
		t.Errorf("rate limit read as %+v, %v", l, ok)
	}
	late := &acpError{message: "Reached free model rate limit. Your limit will reset in 1 minute (at 00:01 UTC)."}
	if l, _ := limitOf(late, time.Date(2026, 10, 30, 23, 59, 30, 0, time.UTC)); l.until.Day() != 31 {
		t.Errorf("a reset past midnight is %v", l.until)
	}
	quota := &acpError{message: "Quota exhausted: Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload."}
	if l, ok := limitOf(quota, now); !ok || l.rate {
		t.Errorf("quota read as %+v, %v", l, ok)
	}
	if _, ok := limitOf(&acpError{message: "no such session"}, now); ok {
		t.Error("an unrelated error is a limit")
	}
}

// A seventh task is not started, and the answer says which six are running and how to wait.
func TestATaskOverTheLimitIsToldToWait(t *testing.T) {
	s := &server{sessions: map[string]*served{}, maxTasks: 2}
	for _, id := range []string{"ses_a", "ses_b"} {
		s.sessions[id] = &served{r: &run{devinSession: id}, session: id, title: "task " + id, busy: true, live: true,
			started: time.Now().Add(-time.Minute), act: &activity{}}
	}
	_, _, err := s.turn("ses_c", "/work", []string{"one more"})
	var noRoom *full
	if !errors.As(err, &noRoom) || len(noRoom.tasks) != 2 {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"at capacity", "ses_a", "task ses_b", "task_status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the answer does not say %q:\n%s", want, err)
		}
	}
	if s.sessions["ses_c"] != nil || s.opening != 0 {
		t.Error("the refused task was started")
	}
}
