package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/k8ika0s/mcp-tmux/go/internal/tmux"
)

// Placeholder main: demonstrates remote tmux execution helper. Replace with gRPC wiring.
func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    out, err := tmux.Run(ctx, "", "tmux", []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"}, []string{"list-sessions"})
    if err != nil {
        log.Printf("tmux run (local) failed: %v", err)
    } else {
        fmt.Printf("tmux list-sessions (local):\n%s\n", out)
    }
    fmt.Println("TODO: wire gRPC service defined in go/proto/tmux.proto (StreamPane, Snapshot, etc.)")
}
