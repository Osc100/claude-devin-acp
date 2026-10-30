package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	_ = flags.Parse(args)

	var state struct {
		Limited bool      `json:"limited"`
		Until   time.Time `json:"until"`
		Message string    `json:"message"`
	}
	if err := get("http://"+*addr+"/limit", &state); err != nil {
		fail(err)
	}
	if !state.Limited {
		fmt.Println("Devin is not rate limited now: go on.")
		return
	}
	if *compact {
		if err := compactSession(os.Getenv("OPENCODE_SESSION_ID")); err != nil {
			fmt.Println("Not compacted:", err)
		} else {
			fmt.Println("Asked OpenCode to compact this session.")
		}
	}
	until := state.Until.Add(limitMargin)
	fmt.Printf("Devin is rate limited until %s; waiting until %s (%s).\n",
		state.Until.Local().Format("Jan 2 15:04"), until.Local().Format("Jan 2 15:04"), time.Until(until).Round(time.Minute))
	time.Sleep(time.Until(until))
	fmt.Println("Devin's rate limit has lifted. Go on with the work, starting again the Devin tasks the limit stopped.")
}

// compactSession asks the OpenCode server this machine runs to compact a session.
func compactSession(session string) error {
	if session == "" {
		return fmt.Errorf("OPENCODE_SESSION_ID is not set: not run by OpenCode")
	}
	state, _ := os.UserHomeDir()
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		state = dir
	} else {
		state = filepath.Join(state, ".local", "state")
	}
	raw, err := os.ReadFile(filepath.Join(state, "opencode", "service.json"))
	if err != nil {
		return fmt.Errorf("no OpenCode server found: %w", err)
	}
	var service struct {
		URL      string `json:"url"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &service); err != nil {
		return err
	}
	request, _ := http.NewRequest(http.MethodPost, service.URL+"/api/session/"+url.PathEscape(session)+"/compact", bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:"+service.Password)))
	if cwd, err := os.Getwd(); err == nil {
		request.Header.Set("x-opencode-directory", url.PathEscape(cwd))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		var message [300]byte
		n, _ := response.Body.Read(message[:])
		return fmt.Errorf("%s: %s", response.Status, message[:n])
	}
	return nil
}
