package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/k8ika0s/mcp-tmux/go/internal/tmux"
	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultCaptureLines = 200
)

var (
	heartbeatInterval = 5 * time.Second
	pollInterval      = 1 * time.Second
)

// Service implements tmuxproto.TmuxServiceServer.
type Service struct {
	tmuxBin string
	pathAdd []string
	run     func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error)
	tmuxproto.UnimplementedTmuxServiceServer
}

func NewService(tmuxBin string, pathAdd []string) *Service {
	return NewServiceWithRunner(tmuxBin, pathAdd, tmux.Run)
}

func NewServiceWithRunner(tmuxBin string, pathAdd []string, runner func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error)) *Service {
	return &Service{tmuxBin: tmuxBin, pathAdd: pathAdd, run: runner}
}

func (s *Service) StreamPane(req *tmuxproto.StreamPaneRequest, stream tmuxproto.TmuxService_StreamPaneServer) error {
	target, pane, err := resolvePaneTarget(req.GetTarget())
	if err != nil {
		return err
	}

	ctx := stream.Context()
	seq := req.FromSeq
	interval := pollInterval
	if req.PollMillis > 0 {
		interval = time.Duration(req.PollMillis) * time.Millisecond
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
	}
	maxBytes := req.MaxChunkBytes
	if maxBytes == 0 {
		maxBytes = 8192
	}

	if target.Host == "" && req.PollMillis == 0 {
		if err := s.streamViaPipe(ctx, stream, target, pane, req.StripAnsi, maxBytes, interval, seq); err == nil {
			return nil
		}
	}

	ticker := time.NewTicker(interval)
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, captureArgs)
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

	sessions, _ := s.run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-sessions"})
	windows, _ := s.run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-windows"})
	panes, _ := s.run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, []string{"list-panes"})
	captureArgs := []string{"capture-pane", "-pJ", "-S", fmt.Sprintf("-%d", captureLines)}
	capture, _ := s.run(ctx, tgt.Host, s.tmuxBin, s.pathAdd, captureArgs)

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
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
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
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, req.Args)
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
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
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
		_, _ = s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"send-keys", "-t", pane, "C-c", "C-u"})
	}

	_, err = s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"send-keys", "-t", pane, cmd, "Enter"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "run batch failed: %v", err)
	}

	resp := &tmuxproto.RunBatchResponse{Text: "batch sent"}
	if req.CaptureLines > 0 {
		captureLines := req.CaptureLines
		args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", captureLines)}
		capOut, capErr := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
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
		out, runErr := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, step.Args)
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

func (s *Service) CaptureLayout(ctx context.Context, req *tmuxproto.CaptureLayoutRequest) (*tmuxproto.CaptureLayoutResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-windows", "-F", "#{window_id}\t#{window_layout}"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list-windows failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	layouts := make([]*tmuxproto.WindowLayout, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		layouts = append(layouts, &tmuxproto.WindowLayout{
			Window: parts[0],
			Layout: parts[1],
		})
	}
	return &tmuxproto.CaptureLayoutResponse{Layouts: layouts}, nil
}

func (s *Service) RestoreLayout(ctx context.Context, req *tmuxproto.RestoreLayoutRequest) (*tmuxproto.RestoreLayoutResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if len(req.Layouts) == 0 {
		return nil, status.Error(codes.InvalidArgument, "layouts are required")
	}
	for _, l := range req.Layouts {
		if l == nil || l.Window == "" || l.Layout == "" {
			continue
		}
		args := []string{"select-layout", "-t", l.Window, l.Layout}
		if _, runErr := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args); runErr != nil {
			log.Printf("restore layout for %s failed: %v", l.Window, runErr)
		}
	}
	return &tmuxproto.RestoreLayoutResponse{Text: "layouts applied"}, nil
}

func (s *Service) NewSession(ctx context.Context, req *tmuxproto.NewSessionRequest) (*tmuxproto.NewSessionResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if target.Session == "" {
		return nil, status.Error(codes.InvalidArgument, "session is required")
	}
	args := []string{"new-session", "-d", "-s", target.Session}
	if req.Command != "" {
		args = append(args, req.Command)
	}
	if _, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args); err != nil {
		return nil, status.Errorf(codes.Internal, "new-session failed: %v", err)
	}
	if req.Attach {
		_, _ = s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"attach-session", "-t", target.Session})
	}
	return &tmuxproto.NewSessionResponse{Text: fmt.Sprintf("session %s created", target.Session)}, nil
}

func (s *Service) NewWindow(ctx context.Context, req *tmuxproto.NewWindowRequest) (*tmuxproto.NewWindowResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if target.Session == "" {
		return nil, status.Error(codes.InvalidArgument, "session is required")
	}
	args := []string{"new-window", "-t", target.Session}
	if req.Name != "" {
		args = append(args, "-n", req.Name)
	}
	if req.Command != "" {
		args = append(args, req.Command)
	}
	if _, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args); err != nil {
		return nil, status.Errorf(codes.Internal, "new-window failed: %v", err)
	}
	return &tmuxproto.NewWindowResponse{Text: "window created"}, nil
}

func (s *Service) ListSessions(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, []string{"list-sessions"})
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
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
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
	out, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, args)
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

var ansiRegex = regexp.MustCompile(`[\x1B\x9B][[\]()#;?]*(?:(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><~])`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func syscallMkfifo(path string, mode uint32) error {
	return unix.Mkfifo(path, mode)
}

func (s *Service) streamViaPipe(ctx context.Context, stream tmuxproto.TmuxService_StreamPaneServer, target *tmuxproto.PaneRef, pane string, strip bool, maxBytes uint32, interval time.Duration, startSeq uint64) error {
	dir, err := os.MkdirTemp("", "tmux-stream-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pipePath := filepath.Join(dir, "pipe")
	if err := syscallMkfifo(pipePath, 0600); err != nil {
		return err
	}

	startArgs := []string{"pipe-pane", "-t", pane, fmt.Sprintf("cat >> %s", pipePath)}
	if _, err := s.run(ctx, target.Host, s.tmuxBin, s.pathAdd, startArgs); err != nil {
		return err
	}
	defer s.run(context.Background(), target.Host, s.tmuxBin, s.pathAdd, []string{"pipe-pane", "-t", pane})

	f, err := os.Open(pipePath)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)

	seq := startSeq
	sendChunk := func(data []byte, heartbeat bool, eof bool, reason string) error {
		seq++
		chunk := &tmuxproto.PaneChunk{
			Target:       target,
			Seq:          seq,
			TsUnixMillis: time.Now().UnixMilli(),
			Data:         data,
			Heartbeat:    heartbeat,
			Eof:          eof,
			Reason:       reason,
		}
		return stream.Send(chunk)
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	done := make(chan error, 1)

	go func() {
		for {
			buf := make([]byte, 4096)
			n, readErr := reader.Read(buf)
			if n > 0 {
				data := buf[:n]
				if strip {
					data = []byte(stripANSI(string(data)))
				}
				for len(data) > 0 {
					chunk := data
					if maxBytes > 0 && len(chunk) > int(maxBytes) {
						chunk = data[:maxBytes]
						data = data[maxBytes:]
					} else {
						data = nil
					}
					if err := sendChunk(chunk, false, false, ""); err != nil {
						done <- err
						return
					}
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					done <- nil
				} else {
					done <- readErr
				}
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			if err != nil {
				return err
			}
			_ = sendChunk(nil, false, true, "eof")
			return nil
		case <-heartbeat.C:
			if err := sendChunk(nil, true, false, ""); err != nil {
				return err
			}
		}
	}
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
