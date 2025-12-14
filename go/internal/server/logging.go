package server

import (
	"context"
	"encoding/json"
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
	json  bool
}

func AuditOptions(color bool, jsonOut bool) []grpc.ServerOption {
	cfg := auditConfig{color: color, json: jsonOut}
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
	args := argsSummary(req)
	if a.json {
		entry := map[string]interface{}{
			"ts":     time.Now().Format(time.RFC3339),
			"method": method,
			"target": target,
			"status": statusText,
			"dur_ms": dur.Milliseconds(),
			"args":   args,
			"stream": stream,
		}
		if err != nil {
			entry["error"] = err.Error()
		}
		if data, e := json.Marshal(entry); e == nil {
			log.Print(string(data))
			return
		}
	}
	msg := fmt.Sprintf("%s %s (%s) %s%s%s%s",
		time.Now().Format(time.RFC3339),
		method,
		target,
		a.wrap(statusColor, statusText),
		a.wrap(colorGray, fmt.Sprintf(" %v", dur)),
		colorReset,
		args,
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

func argsSummary(req interface{}) string {
	switch v := req.(type) {
	case *tmuxproto.RunCommandRequest:
		return fmt.Sprintf(" args=%v", v.Args)
	case *tmuxproto.RunBatchRequest:
		return fmt.Sprintf(" steps=%d", len(v.Steps))
	case *tmuxproto.MultiRunRequest:
		return fmt.Sprintf(" runs=%d", len(v.Steps))
	case *tmuxproto.BatchCaptureRequest:
		return fmt.Sprintf(" captures=%d", len(v.Requests))
	case *tmuxproto.TailPaneRequest:
		return fmt.Sprintf(" tail lines=%d", v.Lines)
	case *tmuxproto.StateRequest:
		return fmt.Sprintf(" state captures=%d", len(v.Targets))
	}
	return ""
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
