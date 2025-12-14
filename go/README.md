# Go refactor (experimental)

This directory contains an experimental Go gRPC scaffold for streaming tmux output.

- Proto: `go/proto/tmux.proto` (StreamPane, Snapshot, list/defaults).
- Helper: `go/internal/tmux/exec.go` wraps tmux (local/ssh) using base64 to preserve `-F '#{...}'` format strings.
- Entry point: `go/cmd/mcp-tmux-go/main.go` (placeholder; shows tmux helper usage).

Next steps:
1) Generate gRPC bindings once `protoc` is available:
   ```bash
   protoc -I go/proto --go_out=go --go-grpc_out=go go/proto/tmux.proto
   ```
2) Wire `TmuxService` using the helper in `internal/tmux` for StreamPane/Snapshot/List*.
3) Add mTLS/token auth and audit logging, mirroring the TypeScript server.

## Running the Go gRPC server (experimental)

1) Ensure `protoc` (>=29) and protoc plugins are available:
   ```bash
   # already installed in this branch when generated stubs
   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.1
   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
   ```

2) Build/run the gRPC server:
   ```bash
   cd go
   go run ./cmd/mcp-tmux-go --listen :9000 --tmux tmux
   ```
   Adjust `--tmux` and PATH additions in `main.go` as needed.

3) gRPC API (from proto):
   - `StreamPane` (stream PaneChunk): live pane output with seq/ts/heartbeat/eof.
   - `Snapshot` (unary): sessions/windows/panes/capture.
   - `ListSessions` / `ListWindows` / `ListPanes` (unary): raw tmux list outputs.
   - `SetDefault` (unary): placeholder for default target management.

Notes:
- StreamPane currently does a one-time capture + heartbeats; replace with pipe-pane tailing for live streaming.
- tmux execution uses base64-wrapped ssh command to preserve format strings (`-F '#{...}'`).
