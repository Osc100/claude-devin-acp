package main

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The recorded Devin session — one subagent started, two files read — as `serve` streams it:
// the subagent as its two lines, the run's own text, and nothing of the subagent's own steps.
func TestARecordedTurnStreamsAsTextAndLines(t *testing.T) {
	recorder := httptest.NewRecorder()
	out := newReply(recorder, false)
	recorded, err := os.Open("testdata/devin-subagent.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer recorded.Close()
	scanner := bufio.NewScanner(recorded)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var msg map[string]any
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg["method"] != "session/update" {
			continue
		}
		update := msg["params"].(map[string]any)["update"].(map[string]any)
		meta, _ := update["_meta"].(map[string]any)
		out.event(event{ownerOf(meta), update})
	}
	out.finish()
	var reply struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	content := reply.Choices[0].Message.Content
	for _, want := range []string{"Subagent (Explore)", "Subagent finished", "5"} {
		if !strings.Contains(content, want) {
			t.Errorf("the reply does not say %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "Read file") {
		t.Errorf("the subagent's own file reads reached the reply:\n%s", content)
	}
	if reply.Choices[0].Message.Reasoning == "" {
		t.Error("Devin's thoughts were lost")
	}
}

func TestTheWorkingDirectoryIsReadOffOpenCodesPrompt(t *testing.T) {
	found := workingDirectory.FindStringSubmatch("x\n<env>\n  Working directory: /home/osc/Projects/x  \n  Platform: linux\n</env>")
	if found == nil || found[1] != "/home/osc/Projects/x" {
		t.Errorf("read %v", found)
	}
}

func TestTheModelSaysTheEffort(t *testing.T) {
	for model, want := range map[string]string{"swe-2-max": "max", "swe-2-medium": "medium", "swe-2-high": "high", "devin": ""} {
		if got := effortOf(chatRequest{Model: model}); got != want {
			t.Errorf("%s asked for %q, not %q", model, got, want)
		}
	}
}

// A message reaches only a Devin that is in a turn; otherwise Claude is told where it went wrong.
func TestAMessageNeedsATaskInATurn(t *testing.T) {
	s := &server{sessions: map[string]*served{"ses_done": {r: &run{}}}}
	for session, want := range map[string]string{"ses_none": "no Devin task", "ses_done": "continue it with the subagent tool"} {
		result := s.call(map[string]any{"name": "message_task", "arguments": map[string]any{"sessionID": session, "message": "stop"}})
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		if result["isError"] != true || !strings.Contains(text, want) {
			t.Errorf("%s: %v", session, result)
		}
	}
}
