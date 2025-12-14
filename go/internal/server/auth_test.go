package server

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestAuthFromContext(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer token123"))
	if got := authFromContext(ctx); got != "token123" {
		t.Fatalf("expected token123, got %q", got)
	}
	ctx2 := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-mcp-token", "abc"))
	if got := authFromContext(ctx2); got != "abc" {
		t.Fatalf("expected abc, got %q", got)
	}
}
