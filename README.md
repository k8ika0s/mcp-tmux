
<p align="center">
  <img src="./mcp_tmux_logo.png" alt="mcp-tmux logo" width="420" />
  <br />
  <em>Remote-first tmux co-pilot for humans + LLMs.</em>
</p>

# mcp-tmux

[![CI](https://github.com/k8ika0s/mcp-tmux/actions/workflows/ci.yml/badge.svg)](https://github.com/k8ika0s/mcp-tmux/actions/workflows/ci.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Node](https://img.shields.io/badge/Node-%3E%3D18-blue.svg)](https://nodejs.org)
[![Dependabot](https://img.shields.io/badge/Dependabot-enabled-025E8C?logo=dependabot)](https://github.com/k8ika0s/mcp-tmux/pulls?q=is%3Apr+author%3Aapp%2Fdependabot)

## Table of Contents
- [Quickstart (2 minutes)](#quickstart-2-minutes)
- [Highlights](#highlights)
- [Prerequisites](#prerequisites)
- [Install & run](#install--run)
- [MCP client wiring (example)](#mcp-client-wiring-example)
- [Exposed tools](#exposed-tools)
- [Collaborative workflow](#collaborative-workflow)
- [How-to (verbose examples)](#how-to-verbose-examples)
- [Configuration](#configuration)
- [Safety notes](#safety-notes)
- [Tips for LLM prompts](#tips-for-llm-prompts)
- [CI, security, and governance](#ci-security-and-governance)
- [Developing](#developing)
- [License](#license)

---

mcp-tmux keeps LLMs grounded in reality instead of guessing which pane they are in. It boots or reattaches SSH-backed tmux sessions, injects keystrokes deliberately, captures scrollback on demand, and keeps defaults so the model doesn’t wander. State is pulled fresh instead of hallucinated.

If you want an AI pair in real terminals—debugging live systems, automating repeatable chores, or co-driving without handing over unlimited shell access—this gives you a precise, observable control surface.

> Guardrails exist (confirm flags, logging, defaults). Sharp edges also exist. Pair responsibly.

## Quickstart (2 minutes)
1) Install & build:
```bash
npm install
npm run build
```
2) Run with sensible defaults:
```bash
MCP_TMUX_HOST=my-ssh-alias MCP_TMUX_SESSION=collab npx mcp-tmux
```
3) In your MCP client, call:
```
tmux.open_session → tmux.default_context → tmux.list_windows / tmux.list_panes → tmux.send_keys → tmux.capture_pane
```
4) Re-ground anytime with `tmux.state`.
## Highlights
- Remote-first: give it an SSH alias + session name and it will create or reconnect the remote tmux session for you.
- Collaborative: you and the model can attach to the same tmux; new windows/panes get LLM-friendly labels.
- Complete control: list, capture, send keys, manage windows/panes/sessions, or fall back to raw tmux commands when needed.
- Safety in the loop: destructive calls require `confirm=true`; defaults keep the model from mis-targeting panes.
- Observability: logging back to the client plus a `tmux.state` snapshot that includes recent scrollback.
- Tasks-ready: built-in MCP task helpers for tailing, watching, and waiting on patterns.

### Capability map
| Pillar | What it covers |
| --- | --- |
| Remote orchestration | `tmux.open_session`, SSH-aware tmux spawn/attach, PATH/tmux-bin overrides, host profiles |
| Grounded control | `tmux.state`, `tmux.capture_pane`, `tmux.list_*`, default targets, pane/window labels |
| Collaboration | `tmux.new_window`, `tmux.split_pane`, sync panes, layout capture/restore, layout profiles |
| Safety | `confirm=true` on destructive calls, logging + audit logs, defaults to avoid mis-targeting |
| Automation | `tmux.multi_run`, tail/pattern/watch tasks, fan-out capture/tail/pattern modes |

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
- `tmux.tail_task`: Task-based tail with polling over time (client polls task results).
- `tmux.select_window` / `tmux.select_pane`: Change focus targets explicitly.
- `tmux.set_sync_panes`: Toggle synchronize-panes for a window.
- `tmux.save_layout_profile` / `tmux.apply_layout_profile`: Persist and re-apply layout profiles by name.
- `tmux.health`: Quick health check (tmux reachable, session listing, host profile info).
- `tmux.context_history`: Pull recent scrollback (pane or session) and extract recent commands.
- `tmux.quickstart`: Return a concise playbook/do-don’t block for the LLM.
- `tmux.multi_run`: Fan-out send + capture/tail/pattern to multiple hosts/panes.
- Resource: `tmux.state_resource` (URI `tmux://state/default`) returns the current default snapshot on read.
- Logging: session logs are appended under `~/.config/mcp-tmux/logs/{host}/{session}/YYYY-MM-DD.log` (override with `MCP_TMUX_LOG_DIR`).
- Audit logging: enable per-session via `tmux.set_audit_logging` to log commands and outputs verbosely (may grow large).
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
- Tail via task (poll results):
  ```json
  {"name":"tmux.tail_task","arguments":{"target":"collab:0.0","lines":200,"iterations":5,"intervalMs":1500}}
  ```
- Fan-out to multiple hosts/panes:
  ```json
  {"name":"tmux.multi_run","arguments":{
    "targets":[
      {"host":"web-1","target":"ops:0.0"},
      {"host":"web-2","target":"ops:0.0"}
    ],
    "keys":"ls -lah /var/log && tail -n 50 app.log",
    "mode":"send_capture",
    "capture":true,
    "captureLines":200,
    "delayMs":500
  }}
  ```
  - Tail mode: set `"mode":"tail"` with `tailIterations`/`tailIntervalMs`.
  - Pattern mode: set `"mode":"pattern"` with `pattern`/`patternFlags`.
- Capture context history and recent commands:
  ```json
  {"name":"tmux.context_history","arguments":{"session":"collab","lines":800,"allPanes":true}}
  ```
- Quickstart playbook for the LLM:
  ```json
  {"name":"tmux.quickstart","arguments":{}}
  ```
- Select window/pane and toggle sync:
  ```json
  {"name":"tmux.select_window","arguments":{"target":"collab:0"}}
  {"name":"tmux.select_pane","arguments":{"target":"collab:0.1"}}
  {"name":"tmux.set_sync_panes","arguments":{"target":"collab:0","on":true}}
  ```
- Capture and restore layouts:
  ```json
  {"name":"tmux.capture_layout","arguments":{"session":"collab"}}
  {"name":"tmux.restore_layout","arguments":{"target":"collab:0","layout":"your-layout-string"}}
  ```
- Save/apply layout profiles:
  ```json
  {"name":"tmux.save_layout_profile","arguments":{"session":"collab","name":"logs"}}
  {"name":"tmux.apply_layout_profile","arguments":{"name":"logs"}}
  ```
- Health check:
  ```json
  {"name":"tmux.health","arguments":{"host":"my-ssh-alias"}}
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
- `MCP_TMUX_TIMEOUT_MS`: Timeout in ms for tmux/ssh invocations (default 15000).
- Defaults: set via `tmux.set_default` or `tmux.select_pane`; tools like `tmux.capture_pane`, `tmux.send_keys`, and tail/pattern tasks fall back to the default pane when `target` is omitted.
- PATH fallbacks: the server automatically adds `/opt/homebrew/bin:/usr/local/bin:/usr/bin` when invoking tmux (local or remote) so Homebrew installs are found.
- Host profiles (optional): `MCP_TMUX_HOSTS_FILE` can point to a JSON file like:
  ```json
  {
    "hashimac": { "pathAdd": ["/opt/homebrew/bin"], "tmuxBin": "/opt/homebrew/bin/tmux", "defaultSession": "ka0s" }
  }
  ```
- Layout profiles (optional): stored at `~/.config/mcp-tmux/layouts.json` by default via `tmux.save_layout_profile`/`tmux.apply_layout_profile`.
- Logging directory: defaults to `~/.config/mcp-tmux/logs` (override with `MCP_TMUX_LOG_DIR`), organized by host/session with daily log files.

## Safety notes
> Safety spotlight: destructive tools need `confirm=true`, and defaults help you avoid targeting the wrong pane. Keep logs on; review captures before acting.

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
- Release: manual workflow `Release (manual)` (workflow dispatch) builds and packs. Inputs:
  - `publish=true|false` (skips publish if no `NPM_TOKEN`, emits warning)
  - `tag` (ref to release, default main)
  - `version` (set explicit version via `npm version <ver> --no-git-tag-version`)
  - `bump` (semver bump `patch|minor|major` if `version` is not provided)
- Branch protection (intended): main should be protected (require PR, no branch deletion). Configure this in repository settings.
- Ownership: CODEOWNERS assigns all files to @k8ika0s.
- Project stats: TypeScript, Node >=18, publishes `mcp-tmux` CLI entrypoint, MCP stdio server.
- Tests: `npm test` (vitest) covers helper path composition.

## Developing
- TypeScript build: `npm run build`
- Linting/formatting: not configured; keep patches small and readable.
- Make targets:
  - `make install` — install dependencies
  - `make build` — compile to `dist/`
  - `make test` — run tests (vitest)
  - `make dev` — hot-reload dev mode
  - `make start` — run compiled server
  - `make clean` — remove `dist/`

## License
AGPL-3.0-only
