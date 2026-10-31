# claude-devin-acp

A small local server that lets [OpenCode](https://opencode.ai) use the Devin CLI as a subagent.

I run Claude inside OpenCode and wanted it to hand longer tasks off to Devin. OpenCode can't drive
an ACP agent directly, so this sits in between: it talks to `devin acp` and gives OpenCode a model
it can call, plus a couple of MCP servers for keeping an eye on subagents while they work.

What you get on `127.0.0.1:47821`:

- `/v1`: Devin as an OpenAI-compatible model (`swe-2-medium`, `swe-2-high`, `swe-2-max`)
- `/mcp`: check on, message or stop running Devin tasks
- `/mcp/subagents`: the same for OpenCode's own subagents (handy for DeepSeek when Devin is rate limited)

It also handles Devin's rate limits and has a second Devin session review permission requests,
so nothing sits waiting for a click.

## Setup

You need the [Devin CLI](https://docs.devin.ai/cli). Grab a binary from
[Releases](https://github.com/Osc100/claude-devin-acp/releases), or build it with Go:

```sh
go build -o ~/.local/bin/claude-devin-acp .   # or: put the release binary there
cp contrib/claude-devin-serve.service ~/.config/systemd/user/
systemctl --user enable --now claude-devin-serve
```

Then point OpenCode at it in `~/.config/opencode/opencode.jsonc`:

```jsonc
"provider": {
  "devin": {
    "npm": "@ai-sdk/openai-compatible",
    "options": { "baseURL": "http://127.0.0.1:47821/v1", "apiKey": "{file:~/.local/state/claude-devin-acp/token}", "timeout": false },
    "models": { "swe-2-high": { "name": "SWE-2 High" } }
  }
},
"agent": {
  "devin": { "mode": "subagent", "model": "devin/swe-2-high", "description": "Devin, for self-contained coding tasks." }
},
"mcp": {
  "servers": {
    "devin": { "type": "remote", "url": "http://127.0.0.1:47821/mcp", "headers": { "Authorization": "Bearer {file:~/.local/state/claude-devin-acp/token}" }, "oauth": false, "codemode": false },
    "subagents": { "type": "remote", "url": "http://127.0.0.1:47821/mcp/subagents", "headers": { "Authorization": "Bearer {file:~/.local/state/claude-devin-acp/token}" }, "oauth": false, "codemode": false }
  }
}
```

Every request needs the token the server writes to `~/.local/state/claude-devin-acp/token` on
its first start (only you can read it), and anything that looks like it came from a browser is
refused, so a web page can't talk to it.

`claude-devin-acp status` shows what's running, and `claude-devin-acp --help` lists the flags.
Logs go to the journal, and Devin's own to `~/.cache/claude-devin-acp/devin.log`.

## License

MIT
