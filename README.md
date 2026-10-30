# claude-devin-acp

Claude, with the local Devin CLI as its subagent, as one ACP agent for an editor such as Zed.

The editor starts this proxy in place of the Claude adapter. Every message between the editor
and Claude passes through unchanged. On top of that, every session Claude opens gets one more MCP
tool, `devin`, which hands a task to `devin acp` and waits for its answer. The editor sees each
Devin run as a subagent of Claude's session, with Devin's own subagents nested under it, through
ACP's [subagents RFD](https://agentclientprotocol.com/rfds/subagents).

```
editor ⇄ claude-devin-acp ⇄ Claude adapter (claude-agent-acp)
               ⇅  (MCP `devin` tool, on 127.0.0.1)
           devin acp
```

## What it relies on

- **Claude adapter**: the one Zed installs from the ACP registry, run as published, signed in
  through Anthropic's own flow. The proxy never sees a credential. Default:
  `node ~/.local/share/zed/external_agents/registry/npx/claude-acp/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js`,
  or `--claude` / `CLAUDE_DEVIN_CLAUDE`.
- **Devin CLI**: `devin acp` (`--devin` / `CLAUDE_DEVIN_DEVIN`). Devin does not speak the RFD. It
  reports subagents in a dialect of its own, which only this proxy translates; `devin.go`'s
  header says what that dialect is, as captured from CLI 3000.11.234.
- **An editor that shows ACP subagents**: Zed 1.27 preview or later with its `acp-beta` flag.
  That flag is staff-only in Zed's own builds, so it takes a build with the flag turned on. With
  any other editor, the tool still works and answers Claude, and nothing is announced.

The proxy answers an editor that asks for ACP v2 with v1. Zed then starts it again over v1, which
is the RFD form this proxy speaks.

## Zed

`settings.json`:

```json
"agent_servers": {
  "Claude + Devin": {
    "type": "custom",
    "command": "/home/osc/Projects/claude-devin-acp/claude-devin-acp"
  }
}
```

Then start a "Claude + Devin" thread and ask Claude to hand something to Devin.

## What is shown

- **The run**: a card under Claude's message. Its thread is Devin's own messages, thoughts and
  tool calls. Stop cancels Devin's turn.
- **Devin's subagents**: cards inside the run's thread, each with its own transcript. They have
  no Stop button: Devin has a way to stop one subagent, but it has not been seen used.
- **Permissions**: Devin's permission requests are asked in the thread of the agent whose tool
  call it is.
- **Answer**: Claude gets Devin's final answer and a run id. Passing the run id as `continue`
  sends a follow-up to the same Devin session.

Not done: re-opening an old thread does not bring back the Devin cards (the RFD's replay on
`session/load`).

## Develop

```sh
mise install                 # Go, pinned in mise.toml
go test ./...                # the translation, replayed from a recorded Devin session
CLAUDE_DEVIN_E2E=1 CLAUDE_DEVIN_E2E_DIR=<scratch repo> go test -run TestEndToEnd -v ./...
                             # the whole pipe with a fake Claude and the real Devin (spends Devin quota)
go build -o claude-devin-acp .
```

Devin's own log goes to `~/.cache/claude-devin-acp/devin.log`; the proxy's goes to its stderr,
which Zed keeps in its log.
