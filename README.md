# claude-devin-acp

Claude, with the local Devin CLI as its subagent, as one ACP agent for an editor such as Zed. With
`--devin-only`, Devin on its own, with its subagents shown as children ("Devin + Devin").

The editor starts this proxy in place of the Claude adapter. Every message between the editor
and Claude passes through unchanged. On top of that, every session Claude opens gets MCP tools for
Devin (`mcp__devin__…` to Claude): `start_task` starts a run of `devin acp` in the background and
returns at once, `wait_for_tasks` collects runs' answers (or only checks on them), `cancel_task`
stops one. Claude's own subagent
tools are taken away and its system prompt says to delegate to Devin (`--claude-subagents` leaves
them). The editor sees each
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
  },
  "Devin + Devin": {
    "type": "custom",
    "command": "/home/osc/Projects/claude-devin-acp/claude-devin-acp",
    "args": ["--devin-only"]
  }
}
```

In a "Claude + Devin" thread, ask Claude to hand something to Devin. A "Devin + Devin" thread is
Devin's own, as Zed's registry Devin would be, but its subagents are shown as cards.

## Devin + Devin

Zed sends the RFD's `subagents` capability, which Devin ignores; Devin reports its subagents only
when told with flags of Cognition's own, and then in a form Zed does not draw. So the proxy adds
the flags to the editor's `initialize` (only when the editor shows subagents) and translates what
comes back. Devin's top agent passes through untouched, being the editor's own thread.

## What is shown

- **The run**: a card under Claude's message. Its thread is Devin's own messages, thoughts and
  tool calls. Stop cancels Devin's turn.
- **Devin's subagents**: cards inside the run's thread, each with its own transcript. They have
  no Stop button: Devin has a way to stop one subagent, but it has not been seen used.
- **Permissions**: Devin's permission requests are asked in the thread of the agent whose tool
  call it is.
- **Answer**: `wait_for_tasks` gives Claude each run's status and, once it is done, its final answer.
  Passing a finished run's id to `start_task` as `continue` sends a follow-up to the same Devin session.
  An MCP server cannot wake Claude when a run ends, so Claude collects runs itself; its prompt
  says to before it ends its turn.

Not done: re-opening an old thread does not bring back the Devin cards (the RFD's replay on
`session/load`).

## Develop

```sh
mise install                 # Go, pinned in mise.toml
go test ./...                # the translation, replayed from a recorded Devin session
CLAUDE_DEVIN_E2E=1 CLAUDE_DEVIN_E2E_DIR=<scratch repo> go test -run 'EndToEnd' -v ./...
                             # both modes against the real Devin, Claude faked (spends Devin quota)
go build -o claude-devin-acp .
```

Devin's own log goes to `~/.cache/claude-devin-acp/devin.log`; the proxy's goes to its stderr,
which Zed keeps in its log.
