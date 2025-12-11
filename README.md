# mcp-tmux

[![CI](https://github.com/k8ika0s/mcp-tmux/actions/workflows/ci.yml/badge.svg)](https://github.com/k8ika0s/mcp-tmux/actions/workflows/ci.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Node](https://img.shields.io/badge/Node-%3E%3D18-blue.svg)](https://nodejs.org)

mcp-tmux is a Model Context Protocol server that lets an LLM co-drive tmux alongside a human, with remote SSH-aware session bootstrapping, safety rails, and pull-based state snapshots. The server can create or reconnect to a remote tmux session via your SSH config, send keys, read scrollback, manage windows/panes, and keep defaults so the model doesn’t get lost. Destructive actions require explicit confirmation, and logging is emitted back to the client for observability.

## Highlights
- Remote-first: give it an SSH alias + session name and it will create or reconnect the remote tmux session for you.
- Collaborative: you and the model can attach to the same tmux; new windows/panes get LLM-friendly labels.
- Complete control: list, capture, send keys, manage windows/panes/sessions, or fall back to raw tmux commands when needed.
- Safety in the loop: destructive calls require `confirm=true`; defaults keep the model from mis-targeting panes.
- Observability: logging back to the client plus a `tmux.state` snapshot that includes recent scrollback.

## Prerequisites
- tmux available on PATH (override with `TMUX_BIN=/path/to/tmux`). For remote flows, tmux must be installed on the remote host.
- Node.js 18+.
- SSH access to the target host(s) using config aliases (the `host` parameter is the ssh config Host).

## Install & run
```bash
npm install
npm run build
MCP_TMUX_HOST=my-ssh-alias MCP_TMUX_SESSION=collab npx mcp-tmux  # optional defaults
```

During development you can use hot reload:
```bash
npm run dev
```

### MCP client wiring (example)
Add to your MCP client config (example for Claude Desktop/CLI style):
```json
{
  "servers": {
    "tmux": {
      "command": "npx",
      "args": ["mcp-tmux"],
      "env": {
        "MCP_TMUX_HOST": "my-ssh-alias",   // optional default host
        "MCP_TMUX_SESSION": "collab"       // optional default session
      }
    }
  }
}
```

SSH quality-of-life: consider enabling ControlMaster/ControlPersist in your ssh config for faster repeated `ssh -T <host> tmux ...` invocations.

## Exposed tools
- `tmux.open_session`: Ensure a remote tmux session exists (create if missing) given `host` (ssh alias) and `session`, and set them as defaults.
- `tmux.default_context`: Shows detected default session and a quick session listing.
- `tmux.state`: Snapshot sessions, windows, panes, and capture of the active/default pane.
- `tmux.set_default` / `tmux.get_default`: Persist or view default host/session/window/pane.
- `tmux.capture_layout` / `tmux.restore_layout`: Save and re-apply window layouts.
- `tmux.tail_pane`: Poll a pane repeatedly to follow output without reissuing commands.
- `tmux.list_sessions`: Enumerate sessions with window/attach counts.
- `tmux.list_windows`: List windows (optionally scoped to a session).
- `tmux.list_panes`: List panes (optionally scoped to a target).
- `tmux.capture_pane`: Read scrollback from a pane (defaults to last ~200 lines).
- `tmux.send_keys`: Send keystrokes to a pane, optionally with Enter.
- `tmux.new_session`: Create a detached session to collaborate in.
- `tmux.new_window`: Create a window inside a session.
- `tmux.split_pane`: Split a pane horizontally/vertically, optionally with a command.
- `tmux.kill_session`, `tmux.kill_window`, `tmux.kill_pane`: Tear down targets (require `confirm=true`).
- `tmux.rename_session`, `tmux.rename_window`: Rename targets.
- `tmux.command`: Raw access to any tmux command/flags for advanced cases.

Targets accept standard tmux notation: `session`, `session:window`, `session:window.pane`, or pane/window IDs. Most tools also accept an optional `host` (ssh alias) and will fall back to `MCP_TMUX_HOST` or whatever `tmux.open_session` last set.

## Collaborative workflow
1) Ensure SSH access to the remote host (configured in `~/.ssh/config` as `Host my-ssh-alias`).
2) From your MCP client, call `tmux.open_session` with `host: "my-ssh-alias"` and `session: "collab"`. It will create the remote tmux session if needed and set it as default.
3) Call `tmux.default_context` to verify layout (uses the default host/session).
4) Drive the remote session with `tmux.send_keys` and read it with `tmux.capture_pane`.
5) The human can attach directly with `ssh -t my-ssh-alias tmux attach -t collab` to collaborate in real time.
6) Re-ground anytime with `tmux.state` to see sessions/windows/panes and the recent scrollback.

## How-to (verbose examples)
- Bootstrap remote session (create if missing) and set defaults:
  ```json
  {"name":"tmux.open_session","arguments":{"host":"my-ssh-alias","session":"collab","command":"cd /srv && bash"}}
  ```
- Snapshot current state (uses defaults):
  ```json
  {"name":"tmux.state","arguments":{"captureLines":200}}
  ```
- Drive a shell and read results:
  ```json
  {"name":"tmux.send_keys","arguments":{"target":"collab:0.0","keys":"ls -lah","enter":true}}
  {"name":"tmux.capture_pane","arguments":{"target":"collab:0.0","start":-200}}
  ```
- Tail a pane to watch output:
  ```json
  {"name":"tmux.tail_pane","arguments":{"target":"collab:0.0","lines":200,"iterations":3,"intervalMs":1000}}
  ```
- Capture and restore layouts:
  ```json
  {"name":"tmux.capture_layout","arguments":{"session":"collab"}}
  {"name":"tmux.restore_layout","arguments":{"target":"collab:0","layout":"your-layout-string"}}
  ```
- Split a pane and label it:
  ```json
  {"name":"tmux.split_pane","arguments":{"target":"collab:0.0","orientation":"horizontal","command":"htop"}}
  ```
- Tear down (requires explicit confirm):
  ```json
  {"name":"tmux.kill_window","arguments":{"host":"my-ssh-alias","target":"collab:1","confirm":true}}
  ```
- Set or view defaults:
  ```json
  {"name":"tmux.set_default","arguments":{"host":"my-ssh-alias","session":"collab","window":"collab:0","pane":"collab:0.0"}}
  {"name":"tmux.get_default","arguments":{}}
  ```

## Configuration
- `MCP_TMUX_SESSION`: Prefer this session when no explicit target is provided.
- `MCP_TMUX_HOST`: Preferred ssh host alias when no explicit host is provided.
- `TMUX_BIN`: Path to the tmux binary (defaults to `tmux`).
- PATH fallbacks: the server automatically adds `/opt/homebrew/bin:/usr/local/bin:/usr/bin` when invoking tmux (local or remote) so Homebrew installs are found.
- Host profiles (optional): `MCP_TMUX_HOSTS_FILE` can point to a JSON file like:
  ```json
  {
    "hashimac": { "pathAdd": ["/opt/homebrew/bin"], "tmuxBin": "/opt/homebrew/bin/tmux", "defaultSession": "ka0s" }
  }
  ```

## Safety notes
- The server never bypasses tmux permissions; it inherits your user account and socket access.
- `tmux.send_keys` will happily run destructive commands—ask for confirmation before altering state or killing sessions/windows.
- Destructive tools (`tmux/kill-*`, destructive `tmux.command`) require `confirm=true`.
- `tmux.command` runs whatever you pass through to tmux; double-check args before using it.
- Captures are pull-only: the model must request `tmux.capture_pane` to read output after sending keys.
- Remote usage depends on SSH trust; the MCP server inherits your SSH agent/keys and runs commands as your user on the remote host.

## Tips for LLM prompts
- Playbook: `tmux.open_session` → `tmux.default_context` → `tmux.list_windows`/`tmux.list_panes` → `tmux.send_keys` → `tmux.capture_pane`.
- Maintain defaults with `tmux.set_default` and re-ground with `tmux.state`.
- Confirm before destructive actions; prefer helper tools over raw `tmux.command`.
- After any change, re-list windows/panes or capture to stay in sync (server is pull-only).

## CI, security, and governance
- CI: GitHub Actions (`CI` workflow) runs `npm run build`.
- Security: dependency audit job (`npm audit --audit-level=high`) runs in CI.
- Branch protection (intended): main should be protected (require PR, no branch deletion). Configure this in repository settings.
- Ownership: CODEOWNERS assigns all files to @k8ika0s.
- Project stats: TypeScript, Node >=18, publishes `mcp-tmux` CLI entrypoint, MCP stdio server.
- Tests: `npm test` (vitest) covers helper path composition.

## Developing
- TypeScript build: `npm run build`
- Linting/formatting: not configured; keep patches small and readable.

## License
AGPL-3.0-only
