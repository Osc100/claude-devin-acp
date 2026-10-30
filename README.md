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

## OpenCode: Devin as a model (`--serve`)

OpenCode drives no ACP agent (its ACP client mode was closed as not planned) but takes any
OpenAI-compatible provider, so `--serve 127.0.0.1:47821` answers chat completions with a Devin
session: Devin's text as content, its thoughts as reasoning, its tool calls and any subagent it
starts as lines. A conversation is known by the session OpenCode names (`x-opencode-session-id`),
else by its first user message and the working directory OpenCode's prompt states, so a follow-up
reaches the same Devin session. Devin is told to do the
task itself. Each turn ends with a line of what it did: files changed, commands, tool calls,
time, and the model Devin says it ran on.

A permission Devin asks for is decided by the permission reviewer: another Devin session, on
SWE-2 at medium, open while any other session is in a turn, which is handed the request and the
asking session's task and answers through `decide_permission`, the one tool of an MCP server at
`/mcp/permissions`. Any other request of its own is refused. Its decisions are kept in
`~/.cache/claude-devin-acp/permissions.json`, and the exact same tool call in the same directory is
decided again from there; a denied session is told why. A shell command that only reads (each part
a known reader such as `tail`, `rg`, `ls`, `git log` or `sed -n '1,9p'`, with no redirect but to
`/dev/null` and no command substitution) is allowed without asking. Only the reviewer's own
decisions and denials are shown.

Devin is always SWE-2 (`swe-2-high`); how hard it thinks is its `thought_level`, set per session:
`--devin-effort` (high) by default, the model `swe-2-medium`, `swe-2-high` or `swe-2-max` (Devin's
own names), or a request's `reasoning_effort`. In Claude + Devin, `start_task` takes an `effort`.

In OpenCode, a `devin` provider and a `devin` subagent on its model (`agent.devin`, `mode:
subagent`, `model: devin/swe-2-high`); Claude, through OpenCode's Claude Agent provider, starts it
with OpenCode's own subagent tool, in the background, and OpenCode shows it as its subagent.

At most `--max-tasks` (6) tasks run at once, since Devin's rate limit is shared by all of them;
the permission reviewer is not counted. One more is not started: its reply says which tasks are
running and to start it again once one finishes.

Limits: Devin retries a rate limit itself, and once its retries are spent it answers the turn
with an error saying when the limit lifts; `serve` then says so, waits until then, and has Devin
go on (up to five times a turn), sessions told the same reset going on 15s apart. A used-up daily quota is reported as that, not waited out.

Watching and steering: `/mcp` has `task_status` (the tasks running and finished in the last hour,
each with the last thing it did), `peek_task` (a task's latest lines), `message_task` and
`stop_task` (tell it to stop now and report, its turn cancelled after a timeout).
`claude-devin-acp status [-f] [-n lines] [session]` shows the same from a shell, as over SSH: the
tasks, or what one has done, followed with `-f`.

Steering: OpenCode holds a message for a busy subagent until its step ends, and a step here is
Devin's whole turn. So the server is also an MCP server at `/mcp`, whose `message_task` takes the
subagent's session id and a message and hands it to Devin at once; Devin takes it as the user
interrupting, and the subagent's reply goes on with what it does then. In OpenCode:
`"mcp": {"servers": {"devin": {"type": "remote", "url": "http://127.0.0.1:47821/mcp", "oauth": false, "codemode": false}}}`.

To keep it running: `contrib/claude-devin-serve.service`, as a user service:

```sh
cp contrib/claude-devin-serve.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now claude-devin-serve
```

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
