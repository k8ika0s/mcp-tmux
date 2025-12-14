package server

import (
	"context"
	"fmt"
	"log"
	"time"

	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

const (
	colorReset = "\033[0m"
	colorGray  = "\033[90m"
	colorCyan  = "\033[36m"
	colorGreen = "\033[32m"
	colorRed   = "\033[31m"
	colorBlue  = "\033[34m"
)

type auditConfig struct {
	color bool
}

func AuditOptions(color bool) []grpc.ServerOption {
	cfg := auditConfig{color: color}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(cfg.unaryAudit()),
		grpc.ChainStreamInterceptor(cfg.streamAudit()),
	}
}

func (a auditConfig) unaryAudit() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		a.log(info.FullMethod, req, start, err, false)
		return resp, err
	}
}

func (a auditConfig) streamAudit() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		a.log(info.FullMethod, nil, start, err, true)
		return err
	}
}

func (a auditConfig) log(method string, req interface{}, start time.Time, err error, stream bool) {
	statusText := "ok"
	statusColor := colorGreen
	if err != nil {
		statusText = grpcStatus(err)
		statusColor = colorRed
	}
	target := targetSummary(req)
	dur := time.Since(start).Truncate(time.Millisecond)
	if stream {
		statusText = "stream"
		if err != nil {
			statusText = grpcStatus(err)
		}
	}
	msg := fmt.Sprintf("%s %s (%s) %s%s%s %s%s",
		time.Now().Format(time.RFC3339),
		method,
		target,
		a.wrap(statusColor, statusText),
		a.wrap(colorGray, fmt.Sprintf(" %v", dur)),
		colorReset,
		"",
		"",
	)
	log.Print(msg)
}

func (a auditConfig) wrap(color string, s string) string {
	if !a.color {
		return s
	}
	return color + s + colorReset
}

// targetSummary best-effort extraction of target fields.
func targetSummary(req interface{}) string {
	type getter interface {
		GetTarget() *tmuxproto.PaneRef
	}
	if g, ok := req.(getter); ok {
		t := g.GetTarget()
		if t == nil {
			return "-"
		}
		return fmt.Sprintf("host=%s session=%s window=%s pane=%s", emptyDash(t.Host), emptyDash(t.Session), emptyDash(t.Window), emptyDash(t.Pane))
	}
	return "-"
}

func grpcStatus(err error) string {
	if err == nil {
		return "ok"
	}
	st, ok := status.FromError(err)
	if !ok {
		return "error"
	}
	return st.Code().String()
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
