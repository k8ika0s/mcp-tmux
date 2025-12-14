# Go refactor (experimental)

Experimental Go gRPC implementation of the tmux MCP server.

- Proto: `proto/tmux.proto` (StreamPane, CapturePane, RunCommand, SendKeys, Snapshot, list/defaults).
- Helper: `internal/tmux/exec.go` wraps tmux locally or over SSH with a base64 shim so `-F '#{...}'` survives remote shells.
- Service: `internal/server/server.go` implements unary helpers plus a polling StreamPane with heartbeats and chunking.
- Entry point: `cmd/mcp-tmux-go/main.go` starts the gRPC server (default `:9000`).

## Running

```bash
cd go
go run ./cmd/mcp-tmux-go --listen :9000 --tmux tmux
```
Tweak `--tmux` and the PATH additions in `main.go` if your tmux binary lives elsewhere.

## Regenerating stubs

Requires `protoc` (>=29) plus plugins:
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.1
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

cd go
protoc --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. proto/tmux.proto
```

## API surface (current)

- `StreamPane`: streaming pane output with seq/ts/heartbeat/eof; optional ANSI stripping and chunk truncation.
-   - Local targets default to a tmux `pipe-pane` stream; remote targets try the same via ssh cat of a remote fifo. Set `poll_millis` to force polling (min ~50ms) or to throttle updates.
- `CapturePane`: tail a pane with a line budget and optional ANSI stripping.
- `RunCommand`: run arbitrary tmux subcommands (raw args) on a host.
- `SendKeys`: send keys (and optionally Enter) to a pane.
- `RunBatch`: join multiple shell steps (default `&&`), optional prompt clean (C-c/C-u), and optional capture after run.
- `MultiRun`: fan out raw tmux commands across multiple targets and aggregate results (with optional ANSI stripping).
- `Snapshot` / `ListSessions` / `ListWindows` / `ListPanes` / `SetDefault`: basic inventory helpers.
- `CaptureLayout` / `RestoreLayout`: snapshot and re-apply window layouts.
- `NewSession` / `NewWindow`: create sessions/windows (optionally run a command or attach).
- Health + reflection: the gRPC server exposes standard health checks and (by default) reflection for easier local integration.

Notes: StreamPane still uses `capture-pane` polling; swapping to `pipe-pane` tailing would provide near-real-time streaming. Auth/z-audit still to be added to mirror the Node MCP server.
