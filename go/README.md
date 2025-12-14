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
