package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/k8ika0s/mcp-tmux/go/internal/tmux"
	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultCaptureLines = 200
	heartbeatInterval   = 5 * time.Second
	pollInterval        = 1 * time.Second
)

// Service implements tmuxproto.TmuxServiceServer.
type Service struct {
	tmuxBin string
	pathAdd []string
	tmuxproto.UnimplementedTmuxServiceServer
}

func NewService(tmuxBin string, pathAdd []string) *Service {
	return &Service{tmuxBin: tmuxBin, pathAdd: pathAdd}
}

func (s *Service) StreamPane(req *tmuxproto.StreamPaneRequest, stream tmuxproto.TmuxService_StreamPaneServer) error {
	if req == nil || req.Target == nil {
		return status.Error(codes.InvalidArgument, "target required")
	}
	target := req.Target
	pane := target.Pane
	if pane == "" && target.Window != "" && target.Session != "" {
		pane = fmt.Sprintf("%s:%s.0", target.Session, target.Window)
	}
	if pane == "" && target.Session != "" {
		pane = fmt.Sprintf("%s.0", target.Session)
	}
	if pane == "" {
		return status.Error(codes.InvalidArgument, "pane required")
	}

	ctx := stream.Context()
	seq := req.FromSeq
	ticker := time.NewTicker(pollInterval)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	defer heartbeat.Stop()

	sendChunk := func(data string, eof bool, reason string) error {
		seq++
		chunk := &tmuxproto.PaneChunk{
			Target:       req.Target,
			Seq:          seq,
			TsUnixMillis: time.Now().UnixMilli(),
			Data:         []byte(data),
			Heartbeat:    data == "" && !eof,
			Eof:          eof,
			Reason:       reason,
		}
		return stream.Send(chunk)
	}

	last := ""
	captureArgs := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", defaultCaptureLines)}
	strip := req.StripAnsi
	maxBytes := req.MaxChunkBytes
	if maxBytes == 0 {
		maxBytes = 8192
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, captureArgs)
			if err != nil {
				return status.Errorf(codes.Internal, "capture failed: %v", err)
			}
			if strip {
				out = stripANSI(out)
			}
			if out != last {
				delta := out
				if strings.HasPrefix(out, last) {
					delta = out[len(last):]
				}
				truncated := false
				if maxBytes > 0 && len(delta) > int(maxBytes) {
					delta = delta[:maxBytes]
					truncated = true
				}
				if err := sendChunk(delta, false, ""); err != nil {
					return err
				}
				if truncated {
					if err := sendChunk("", false, "truncated"); err != nil {
						return err
					}
				}
				last = out
			}
		case <-heartbeat.C:
			if err := sendChunk("", false, ""); err != nil {
				return err
			}
		}
	}
}

func (s *Service) Snapshot(ctx context.Context, req *tmuxproto.SnapshotRequest) (*tmuxproto.SnapshotResponse, error) {
	if req == nil || req.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	tgt := req.Target
	captureLines := req.CaptureLines
	if captureLines == 0 {
		captureLines = defaultCaptureLines
	}

	sessions, _ := tmux.Run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-sessions"})
	windows, _ := tmux.Run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-windows"})
	panes, _ := tmux.Run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-panes"})
	captureArgs := []string{"capture-pane", "-pJ", "-S", fmt.Sprintf("-%d", captureLines)}
	capture, _ := tmux.Run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, captureArgs)

	return &tmuxproto.SnapshotResponse{Sessions: sessions, Windows: windows, Panes: panes, Capture: capture}, nil
}

func (s *Service) ListSessions(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	if req == nil || req.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	out, err := tmux.Run(ctx, req.Target.Host, s.tmuxBin, s.pathAdd, []string{"list-sessions"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListWindows(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	if req == nil || req.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	args := []string{"list-windows"}
	if req.Target.Session != "" {
		args = append(args, "-t", req.Target.Session)
	}
	out, err := tmux.Run(ctx, req.Target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListPanes(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	if req == nil || req.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	args := []string{"list-panes"}
	if req.Target.Session != "" {
		args = append(args, "-t", req.Target.Session)
	}
	out, err := tmux.Run(ctx, req.Target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) SetDefault(ctx context.Context, req *tmuxproto.SetDefaultRequest) (*tmuxproto.SetDefaultResponse, error) {
	if req == nil || req.Target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	msg := fmt.Sprintf("Defaults set host=%s session=%s window=%s pane=%s", req.Target.Host, req.Target.Session, req.Target.Window, req.Target.Pane)
	return &tmuxproto.SetDefaultResponse{Text: msg}, nil
}

var ansiRegex = regexp.MustCompile(`[\u001B\u009B][[\]()#;?]*(?:(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><~])`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}
