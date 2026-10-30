package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
)

// Devin + Devin (`--devin-only`): the editor talks to Devin itself, through the proxy, and Devin's
// own subagents are shown as the RFD's children of the thread. The editor cannot send Devin the
// flags that make it report its subagents, and would not understand Devin's report if it could,
// so the proxy does both: it adds the flags to the editor's `initialize`, and turns Devin's
// subagent markers into `subagent_update`s and its subagents' traffic into their own sessions.
// Everything of Devin's top agent passes through untouched, since that thread is the editor's own.

func runSolo(args []string) {
	p := &proxy{editor: &wire{w: os.Stdout}, children: map[string]*run{}, waiting: map[string]chan map[string]any{}}
	d := newDevin(args, p)
	p.devin = d
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stderr = devinLog()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("starting Devin (%s): %v", strings.Join(args, " "), err)
	}
	d.in = &wire{w: stdin}
	go func() {
		p.pump(stdout, d.soloFromDevin)
		log.Print("Devin closed its output")
		os.Exit(0)
	}()
	p.pump(os.Stdin, d.soloFromEditor)
	log.Print("the editor closed its input")
	_ = cmd.Process.Kill()
}

func (d *devin) soloFromEditor(line []byte) {
	msg, ok := decode(line)
	if !ok {
		d.in.sendRaw(line)
		return
	}
	params, _ := msg["params"].(map[string]any)
	switch msg["method"] {
	case "initialize":
		if params == nil {
			break
		}
		if version, _ := params["protocolVersion"].(float64); version > 1 {
			params["protocolVersion"] = 1
		}
		caps, _ := params["clientCapabilities"].(map[string]any)
		// Only for an editor that shows subagents: told to, Devin streams its subagents' work on
		// the parent's thread, which an editor without the RFD would show as the parent's own.
		if sub, present := caps["subagents"]; present && sub != nil {
			d.p.mu.Lock()
			d.p.subagents = true
			d.p.mu.Unlock()
			meta, _ := caps["_meta"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
			}
			meta["cognition.ai/subagentSupport"] = true
			meta["cognition.ai/subagentControl"] = true
			caps["_meta"] = meta
		}
		d.in.send(msg)
		return
	case "session/cancel":
		// A child's: none is offered a Stop, and Devin would not know the session.
		if session, _ := params["sessionId"].(string); strings.Contains(session, "/") {
			return
		}
	}
	d.in.sendRaw(line)
}

// soloRun is the run a Devin session of the editor's own is, made on its first update: the
// session is its own root, announced to nobody.
func (d *devin) soloRun(session string) *run {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runs[session]
	if r == nil {
		r = &run{child: session, devinSession: session, shown: true, agents: map[string]string{rootAgent: session}}
		d.runs[session] = r
	}
	return r
}

func (d *devin) soloFromDevin(line []byte) {
	if !d.p.showsSubagents() {
		d.p.editor.sendRaw(line)
		return
	}
	msg, ok := decode(line)
	if !ok {
		d.p.editor.sendRaw(line)
		return
	}
	params, _ := msg["params"].(map[string]any)
	session, _ := params["sessionId"].(string)
	switch msg["method"] {
	case "session/update":
		update, _ := params["update"].(map[string]any)
		meta, _ := update["_meta"].(map[string]any)
		if meta[metaStarted] == nil && meta[metaCompleted] == nil && ownerOf(meta) == rootAgent {
			d.p.editor.sendRaw(line)
			return
		}
		d.update(map[string]any{"sessionId": session, "update": update}, d.soloRun(session))
		return
	case "session/request_permission":
		call, _ := params["toolCall"].(map[string]any)
		meta, _ := call["_meta"].(map[string]any)
		if owner := ownerOf(meta); owner != rootAgent {
			params["sessionId"] = d.soloRun(session).sessionOf(owner)
			d.p.editor.send(msg)
			return
		}
	}
	d.p.editor.sendRaw(line)
}
