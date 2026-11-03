package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// runWaitLimit is `claude-devin-acp wait-limit`: for an OpenCode session whose Devin subagents
// stopped on a rate limit too long to wait out within a turn. Run as a background shell once they
// have all finished, it has OpenCode compact the session (OPENCODE_SESSION_ID, which OpenCode gives
// every shell command), waits until the limit lifts and a minute more, and exits: OpenCode tells
// the agent a background shell finished, and it goes on.
func runWaitLimit(args []string) {
	flags := flag.NewFlagSet("wait-limit", flag.ExitOnError)
	addr := flags.String("addr", envOr("CLAUDE_DEVIN_SERVE", "127.0.0.1:47821"), "the address serve listens on")
	compact := flags.Bool("compact", true, "have OpenCode compact the session first")
	model := flags.String("model", sweModel, "the model whose limit to wait out: "+sweModel+" (any swe-2-*) or "+lunaHigh)
	_ = flags.Parse(args)
	name := modelName(modelOf(chatRequest{Model: *model}))

	var state struct {
		Limited bool      `json:"limited"`
		Until   time.Time `json:"until"`
		Message string    `json:"message"`
	}
	if err := get("http://"+*addr+"/limit?model="+url.QueryEscape(*model), &state); err != nil {
		fail(err)
	}
	if !state.Limited {
		fmt.Printf("Devin's %s is not rate limited now: go on.\n", name)
		return
	}
	if *compact {
		if err := compactSession(*addr, os.Getenv("OPENCODE_SESSION_ID")); err != nil {
			fmt.Println("Not compacted:", err)
		} else {
			fmt.Println("Asked OpenCode to compact this session.")
		}
	}
	until := state.Until.Add(limitMargin)
	fmt.Printf("Devin's %s is rate limited until %s; waiting until %s (%s).\n", name,
		state.Until.Local().Format("Jan 2 15:04"), until.Local().Format("Jan 2 15:04"), time.Until(until).Round(time.Minute))
	time.Sleep(time.Until(until))
	fmt.Printf("Devin's %s rate limit has lifted. Go on with the work, starting again the Devin tasks the limit stopped.\n", name)
}

// compactSession has the server ask OpenCode to compact a session.
func compactSession(addr, session string) error {
	if session == "" {
		return fmt.Errorf("OPENCODE_SESSION_ID is not set: not run by OpenCode")
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/compact/"+url.PathEscape(session), strings.NewReader("{}"))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("is the server running? %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}
