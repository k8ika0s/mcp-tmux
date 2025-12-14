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
	target, pane, err := resolvePaneTarget(req.GetTarget())
	if err != nil {
		return err
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
			Target:       target,
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
	tgt, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
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

func (s *Service) CapturePane(ctx context.Context, req *tmuxproto.CapturePaneRequest) (*tmuxproto.CapturePaneResponse, error) {
	target, pane, err := resolvePaneTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	lines := req.Lines
	if lines <= 0 {
		lines = defaultCaptureLines
	}
	args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", lines)}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "capture failed: %v", err)
	}
	if req.StripAnsi {
		out = stripANSI(out)
	}
	truncated := false
	if lineCount := strings.Count(out, "\n") + 1; int32(lineCount) >= lines {
		truncated = true
	}
	return &tmuxproto.CapturePaneResponse{Text: out, Truncated: truncated}, nil
}

func (s *Service) RunCommand(ctx context.Context, req *tmuxproto.RunCommandRequest) (*tmuxproto.RunCommandResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if len(req.Args) == 0 {
		return nil, status.Error(codes.InvalidArgument, "args are required")
	}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, req.Args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "tmux %v failed: %v", req.Args, err)
	}
	if req.StripAnsi {
		out = stripANSI(out)
	}
	return &tmuxproto.RunCommandResponse{Text: out}, nil
}

func (s *Service) SendKeys(ctx context.Context, req *tmuxproto.SendKeysRequest) (*tmuxproto.SendKeysResponse, error) {
	target, pane, err := resolvePaneTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if len(req.Keys) == 0 && !req.Enter {
		return nil, status.Error(codes.InvalidArgument, "keys or enter required")
	}
	args := []string{"send-keys", "-t", pane}
	args = append(args, req.Keys...)
	if req.Enter {
		args = append(args, "Enter")
	}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "send-keys failed: %v", err)
	}
	if out == "" {
		out = "(no output)"
	}
	return &tmuxproto.SendKeysResponse{Text: out}, nil
}

func (s *Service) RunBatch(ctx context.Context, req *tmuxproto.RunBatchRequest) (*tmuxproto.RunBatchResponse, error) {
	target, pane, err := resolvePaneTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if len(req.Steps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "steps are required")
	}
	joiner := req.JoinWith
	if joiner == "" {
		joiner = "&&"
	}
	cmd := strings.Join(req.Steps, fmt.Sprintf(" %s ", joiner))

	if req.CleanPrompt {
		_, _ = tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"send-keys", "-t", pane, "C-c", "C-u"})
	}

	_, err = tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"send-keys", "-t", pane, cmd, "Enter"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "run batch failed: %v", err)
	}

	resp := &tmuxproto.RunBatchResponse{Text: "batch sent"}
	if req.CaptureLines > 0 {
		captureLines := req.CaptureLines
		args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", captureLines)}
		capOut, capErr := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
		if capErr == nil {
			if req.StripAnsi {
				capOut = stripANSI(capOut)
			}
			resp.Capture = capOut
			if lineCount := strings.Count(capOut, "\n") + 1; int32(lineCount) >= captureLines {
				resp.Truncated = true
			}
		}
	}
	return resp, nil
}

func (s *Service) MultiRun(ctx context.Context, req *tmuxproto.MultiRunRequest) (*tmuxproto.MultiRunResponse, error) {
	if len(req.Steps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "steps are required")
	}
	results := make([]*tmuxproto.MultiRunResult, 0, len(req.Steps))
	for _, step := range req.Steps {
		target, err := requireTarget(step.GetTarget())
		if err != nil {
			results = append(results, &tmuxproto.MultiRunResult{Target: step.GetTarget(), Error: err.Error()})
			continue
		}
		if len(step.Args) == 0 {
			results = append(results, &tmuxproto.MultiRunResult{Target: target, Error: "args are required"})
			continue
		}
		out, runErr := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, step.Args)
		if runErr != nil {
			results = append(results, &tmuxproto.MultiRunResult{Target: target, Error: runErr.Error()})
			continue
		}
		if req.StripAnsi {
			out = stripANSI(out)
		}
		results = append(results, &tmuxproto.MultiRunResult{Target: target, Text: out})
	}
	return &tmuxproto.MultiRunResponse{Results: results}, nil
}

func (s *Service) ListSessions(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"list-sessions"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListWindows(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-windows"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListPanes(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-panes"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := tmux.Run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) SetDefault(ctx context.Context, req *tmuxproto.SetDefaultRequest) (*tmuxproto.SetDefaultResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Defaults set host=%s session=%s window=%s pane=%s", target.Host, target.Session, target.Window, target.Pane)
	return &tmuxproto.SetDefaultResponse{Text: msg}, nil
}

var ansiRegex = regexp.MustCompile(`[\u001B\u009B][[\]()#;?]*(?:(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><~])`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func requireTarget(target *tmuxproto.PaneRef) (*tmuxproto.PaneRef, error) {
	if target == nil {
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	return target, nil
}

func resolvePaneTarget(target *tmuxproto.PaneRef) (*tmuxproto.PaneRef, string, error) {
	target, err := requireTarget(target)
	if err != nil {
		return nil, "", err
	}
	pane := target.Pane
	if pane == "" && target.Window != "" && target.Session != "" {
		pane = fmt.Sprintf("%s:%s.0", target.Session, target.Window)
	}
	if pane == "" && target.Session != "" {
		pane = fmt.Sprintf("%s.0", target.Session)
	}
	if pane == "" {
		return nil, "", status.Error(codes.InvalidArgument, "pane required")
	}
	return target, pane, nil
}
