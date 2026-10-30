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
	for _, want := range []string{"⚙ Devin subagent (Explore)", "✓ subagent finished", "5"} {
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
