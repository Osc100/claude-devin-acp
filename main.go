// Command claude-devin-acp serves Devin, the local Devin CLI, to OpenCode: as a model, through an
// OpenAI-compatible chat endpoint whose every reply is a Devin session (`serve.go`), and as MCP
// servers that watch and steer the subagents working on that model and on any other
// (`tasks.go`, `opencode.go`). Devin is driven over ACP (`devin acp`), whose dialect `devin.go`
// speaks.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		runStatus(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "wait-limit" {
		runWaitLimit(os.Args[2:])
		return
	}
	listen := flag.String("listen", envOr("CLAUDE_DEVIN_SERVE", "127.0.0.1:47821"), "the address to serve OpenCode on")
	devinCmd := flag.String("devin", envOr("CLAUDE_DEVIN_DEVIN", "devin acp"), "command that starts Devin as an ACP agent")
	devinIdle := flag.Duration("devin-idle", 10*time.Minute, "stop Devin after this long with no turn running (0 keeps it); its sessions are loaded again on the next turn")
	devinEffort := flag.String("devin-effort", "high", "how hard Devin (SWE-2) thinks when a turn asks for nothing else: medium, high or max")
	maxTasks := flag.Int("max-tasks", 6, "how many Devin tasks may run at once (0 for no limit); one more is answered with how to wait for room")
	flag.Parse()
	if !efforts[*devinEffort] {
		fmt.Fprintf(os.Stderr, "--devin-effort: no effort %q: medium, high or max\n", *devinEffort)
		os.Exit(2)
	}
	log.SetPrefix("claude-devin-acp: ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	runServe(*listen, strings.Fields(*devinCmd), *devinIdle, *devinEffort, *maxTasks)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// wire is Devin's input, written a whole line at a time, since ACP over stdio is one JSON-RPC
// message a line and two goroutines may write at once.
type wire struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *wire) send(msg any) {
	line, err := json.Marshal(msg)
	if err != nil {
		log.Printf("encoding a message: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		log.Printf("writing a message: %v", err)
	}
}

// pump hands each line r reads to handle.
func pump(r io.Reader, handle func([]byte)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		handle(append([]byte(nil), scanner.Bytes()...))
	}
}

func decode(line []byte) (map[string]any, bool) {
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, false
	}
	return msg, true
}

func idKey(id any) string { return fmt.Sprint(id) }
