package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
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
type RunMeta struct {
	PackageName string
	Version     string
	RepoURL     string
}

func (s *Service) State(ctx context.Context, req *tmuxproto.StateRequest) (*tmuxproto.StateResponse, error) {
	captureLines := req.CaptureLines
	if captureLines == 0 {
		captureLines = defaultCaptureLines
	}
	captures := []*tmuxproto.PaneCapture{}

	targets := req.Targets
	if len(targets) == 0 && s.defaultTarget != nil {
		targets = []*tmuxproto.PaneRef{s.defaultTarget}
	}
	for _, t := range targets {
		if t == nil {
			continue
		}
		resolved, pane, err := s.resolvePaneTarget(t)
		if err != nil {
			continue
		}
		args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", captureLines)}
		cap, err := s.runTmux(ctx, resolved.Host, args)
		if err != nil {
			continue
		}
		if req.StripAnsi {
			cap = stripANSI(cap)
		}
		trunc := strings.Count(cap, "\n")+1 >= int(captureLines)
		capt := &tmuxproto.PaneCapture{
			Target:         resolved,
			Text:           cap,
			Truncated:      trunc,
			RequestedLines: uint32(captureLines),
		}
		captures = append(captures, capt)
	}

	host := ""
	if len(captures) > 0 {
		host = captures[0].Target.GetHost()
	}
	sessions, _ := s.runTmux(ctx, host, []string{"list-sessions"})
	windows, _ := s.runTmux(ctx, host, []string{"list-windows"})
	panes, _ := s.runTmux(ctx, host, []string{"list-panes"})

	return &tmuxproto.StateResponse{
		Captures: captures,
		Sessions: sessions,
		Windows:  windows,
		Panes:    panes,
	}, nil
}

type hostProfile struct {
	PathAdd        []string `json:"pathAdd"`
	TmuxBin        string   `json:"tmuxBin"`
	DefaultSession string   `json:"defaultSession"`
	DefaultPane    string   `json:"defaultPane"`
}

type defaultTargetStore struct {
	Host    string `json:"host"`
	Session string `json:"session"`
	Window  string `json:"window"`
	Pane    string `json:"pane"`
}

type Service struct {
	tmuxBin       string
	pathAdd       []string
	meta          RunMeta
	hostProfiles  map[string]hostProfile
	defaultsPath  string
	defaultTarget *tmuxproto.PaneRef
	run           func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error)
	tmuxproto.UnimplementedTmuxServiceServer
}

func NewService(tmuxBin string, pathAdd []string) *Service {
	return NewServiceWithRunner(tmuxBin, pathAdd, tmux.Run, RunMeta{
		PackageName: "github.com/k8ika0s/mcp-tmux/go",
		Version:     "dev",
		RepoURL:     "https://github.com/k8ika0s/mcp-tmux",
	})
}

func NewServiceWithRunner(tmuxBin string, pathAdd []string, runner func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error), meta RunMeta) *Service {
	hp := loadHostProfiles()
	defPath, defTarget := loadDefaultTarget()
	return &Service{
		tmuxBin:       tmuxBin,
		pathAdd:       pathAdd,
		hostProfiles:  hp,
		defaultsPath:  defPath,
		defaultTarget: defTarget,
		meta: RunMeta{
			PackageName: meta.PackageName,
			Version:     meta.Version,
			RepoURL:     meta.RepoURL,
		},
		run: runner,
	}
}

// MakeRunnerWithMeta wraps tmux.Run with metadata for convenience.
func MakeRunnerWithMeta(meta RunMeta) func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error) {
	return func(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error) {
		return tmux.Run(ctx, host, tmuxBin, pathAdd, args)
	}
}

func (s *Service) runTmux(ctx context.Context, host string, args []string) (string, error) {
	bin, pathAdd := s.tmuxBin, s.pathAdd
	if hp, ok := s.hostProfiles[host]; ok {
		if hp.TmuxBin != "" {
			bin = hp.TmuxBin
		}
		if len(hp.PathAdd) > 0 {
			pathAdd = append(pathAdd, hp.PathAdd...)
		}
	}
	return s.run(ctx, host, bin, pathAdd, args)
}

func loadHostProfiles() map[string]hostProfile {
	path := os.Getenv("MCP_TMUX_HOSTS_FILE")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "mcp-tmux", "hosts.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]hostProfile{}
	}
	var profiles map[string]hostProfile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return map[string]hostProfile{}
	}
	return profiles
}

func loadDefaultTarget() (string, *tmuxproto.PaneRef) {
	path := os.Getenv("MCP_TMUX_DEFAULTS_FILE")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "mcp-tmux", "defaults.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, nil
	}
	var store defaultTargetStore
	if err := json.Unmarshal(data, &store); err != nil {
		return path, nil
	}
	if store.Session == "" && store.Pane == "" && store.Window == "" && store.Host == "" {
		return path, nil
	}
	return path, &tmuxproto.PaneRef{
		Host:    store.Host,
		Session: store.Session,
		Window:  store.Window,
		Pane:    store.Pane,
	}
}

func persistDefaultTarget(path string, target *tmuxproto.PaneRef) {
	if path == "" || target == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	store := defaultTargetStore{
		Host:    target.Host,
		Session: target.Session,
		Window:  target.Window,
		Pane:    target.Pane,
	}
	if data, err := json.MarshalIndent(store, "", "  "); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

func (s *Service) StreamPane(req *tmuxproto.StreamPaneRequest, stream tmuxproto.TmuxService_StreamPaneServer) error {
	target, pane, err := s.resolvePaneTarget(req.GetTarget())
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

	if req.PollMillis == 0 {
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
			out, err := s.runTmux(ctx, target.Host, captureArgs)
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
	tgt, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	captureLines := req.CaptureLines
	if captureLines == 0 {
		captureLines = defaultCaptureLines
	}

	sessions, _ := s.runTmux(ctx, tgt.Host, []string{"list-sessions"})
	windows, _ := s.runTmux(ctx, tgt.Host, []string{"list-windows"})
	panes, _ := s.runTmux(ctx, tgt.Host, []string{"list-panes"})
	captureArgs := []string{"capture-pane", "-pJ", "-S", fmt.Sprintf("-%d", captureLines)}
	capture, _ := s.runTmux(ctx, tgt.Host, captureArgs)
	trunc := false
	if strings.Count(capture, "\n")+1 >= int(captureLines) {
		trunc = true
	}

	return &tmuxproto.SnapshotResponse{Sessions: sessions, Windows: windows, Panes: panes, Capture: capture, CaptureTruncated: trunc, CaptureRequestedLines: uint32(captureLines)}, nil
}

func (s *Service) CapturePane(ctx context.Context, req *tmuxproto.CapturePaneRequest) (*tmuxproto.CapturePaneResponse, error) {
	target, pane, err := s.resolvePaneTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	lines := req.Lines
	if lines <= 0 {
		lines = defaultCaptureLines
	}
	start := fmt.Sprintf("-%d", lines)
	if req.GetStart() != 0 {
		start = fmt.Sprintf("%d", req.GetStart())
	}
	args := []string{"capture-pane", "-pJ", "-t", pane, "-S", start, "-N", fmt.Sprintf("%d", lines)}
	out, err := s.runTmux(ctx, target.Host, args)
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
	return &tmuxproto.CapturePaneResponse{Target: target, Text: out, Truncated: truncated, RequestedLines: uint32(lines)}, nil
}

func (s *Service) BatchCapture(ctx context.Context, req *tmuxproto.BatchCaptureRequest) (*tmuxproto.BatchCaptureResponse, error) {
	if len(req.Requests) == 0 {
		return nil, status.Error(codes.InvalidArgument, "requests are required")
	}
	caps := make([]*tmuxproto.CapturePaneResponse, 0, len(req.Requests))
	for _, r := range req.Requests {
		capResp, err := s.CapturePane(ctx, r)
		if err != nil {
			return nil, err
		}
		caps = append(caps, capResp)
	}
	return &tmuxproto.BatchCaptureResponse{Captures: caps}, nil
}

func (s *Service) TailPane(req *tmuxproto.TailPaneRequest, stream tmuxproto.TmuxService_TailPaneServer) error {
	target, pane, err := s.resolvePaneTarget(req.GetTarget())
	if err != nil {
		return err
	}
	lines := req.Lines
	if lines == 0 {
		lines = 20
	}
	maxBytes := req.MaxBytes
	if maxBytes == 0 {
		maxBytes = 8192
	}
	interval := 1 * time.Second
	if req.PollMillis > 0 {
		interval = time.Duration(req.PollMillis) * time.Millisecond
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
	}
	budgets := req.LineBudgets
	if len(budgets) == 0 {
		budgets = []uint32{20, 100, 400}
	}
	seq := uint64(0)
	send := func(data []byte, heartbeat bool, eof bool, reason string) error {
		seq++
		return stream.Send(&tmuxproto.TailChunk{
			Target:       target,
			Seq:          seq,
			TsUnixMillis: time.Now().UnixMilli(),
			Data:         data,
			Heartbeat:    heartbeat,
			Eof:          eof,
			Reason:       reason,
		})
	}

	ctx := stream.Context()
	poll := time.NewTicker(interval)
	defer poll.Stop()
	last := ""
	budgetIdx := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			currentLines := lines
			if budgetIdx < len(budgets) {
				currentLines = budgets[budgetIdx]
				budgetIdx++
			}
			args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", currentLines), "-N", fmt.Sprintf("%d", currentLines)}
			out, err := s.runTmux(ctx, target.Host, args)
			if err != nil {
				return status.Errorf(codes.Internal, "tail failed: %v", err)
			}
			if req.StripAnsi {
				out = stripANSI(out)
			}
			if out != last {
				diff := out
				if strings.HasPrefix(out, last) {
					diff = out[len(last):]
				}
				for len(diff) > 0 {
					chunk := diff
					if len(chunk) > int(maxBytes) {
						chunk = diff[:maxBytes]
						diff = diff[maxBytes:]
					} else {
						diff = ""
					}
					if err := send([]byte(chunk), false, false, ""); err != nil {
						return err
					}
				}
				last = out
			} else {
				_ = send(nil, true, false, "")
			}
		}
	}
}

func (s *Service) RunCommand(ctx context.Context, req *tmuxproto.RunCommandRequest) (*tmuxproto.RunCommandResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	if len(req.Args) == 0 {
		return nil, status.Error(codes.InvalidArgument, "args are required")
	}
	if isDestructive(req.Args) && !req.Confirm {
		return nil, status.Error(codes.InvalidArgument, "confirm=true required for destructive commands")
	}
	out, err := s.runTmux(ctx, target.Host, req.Args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "tmux %v failed: %v", req.Args, err)
	}
	if req.StripAnsi {
		out = stripANSI(out)
	}
	return &tmuxproto.RunCommandResponse{Text: out}, nil
}

func (s *Service) SendKeys(ctx context.Context, req *tmuxproto.SendKeysRequest) (*tmuxproto.SendKeysResponse, error) {
	target, pane, err := s.resolvePaneTarget(req.GetTarget())
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
	out, err := s.runTmux(ctx, target.Host, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "send-keys failed: %v", err)
	}
	if out == "" {
		out = "(no output)"
	}
	return &tmuxproto.SendKeysResponse{Text: out}, nil
}

func (s *Service) RunBatch(ctx context.Context, req *tmuxproto.RunBatchRequest) (*tmuxproto.RunBatchResponse, error) {
	target, pane, err := s.resolvePaneTarget(req.GetTarget())
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
		_, _ = s.runTmux(ctx, target.Host, []string{"send-keys", "-t", pane, "C-c", "C-u"})
	}

	_, err = s.runTmux(ctx, target.Host, []string{"send-keys", "-t", pane, cmd, "Enter"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "run batch failed: %v", err)
	}

	resp := &tmuxproto.RunBatchResponse{Text: "batch sent"}
	if req.CaptureLines > 0 {
		captureLines := req.CaptureLines
		args := []string{"capture-pane", "-pJ", "-t", pane, "-S", fmt.Sprintf("-%d", captureLines)}
		capOut, capErr := s.runTmux(ctx, target.Host, args)
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
		target, err := s.requireTarget(step.GetTarget())
		if err != nil {
			results = append(results, &tmuxproto.MultiRunResult{Target: step.GetTarget(), Error: err.Error()})
			continue
		}
		if len(step.Args) == 0 {
			results = append(results, &tmuxproto.MultiRunResult{Target: target, Error: "args are required"})
			continue
		}
		out, runErr := s.runTmux(ctx, target.Host, step.Args)
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
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-windows", "-F", "#{window_id}\t#{window_layout}"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := s.runTmux(ctx, target.Host, args)
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
	target, err := s.requireTarget(req.GetTarget())
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
		if _, runErr := s.runTmux(ctx, target.Host, args); runErr != nil {
			log.Printf("restore layout for %s failed: %v", l.Window, runErr)
		}
	}
	return &tmuxproto.RestoreLayoutResponse{Text: "layouts applied"}, nil
}

func (s *Service) NewSession(ctx context.Context, req *tmuxproto.NewSessionRequest) (*tmuxproto.NewSessionResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
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
	if _, err := s.runTmux(ctx, target.Host, args); err != nil {
		return nil, status.Errorf(codes.Internal, "new-session failed: %v", err)
	}
	if req.Attach {
		_, _ = s.runTmux(ctx, target.Host, []string{"attach-session", "-t", target.Session})
	}
	return &tmuxproto.NewSessionResponse{Text: fmt.Sprintf("session %s created", target.Session)}, nil
}

func (s *Service) NewWindow(ctx context.Context, req *tmuxproto.NewWindowRequest) (*tmuxproto.NewWindowResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
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
	if _, err := s.runTmux(ctx, target.Host, args); err != nil {
		return nil, status.Errorf(codes.Internal, "new-window failed: %v", err)
	}
	return &tmuxproto.NewWindowResponse{Text: "window created"}, nil
}

func (s *Service) ServerInfo(ctx context.Context, req *tmuxproto.ServerInfoRequest) (*tmuxproto.ServerInfoResponse, error) {
	return &tmuxproto.ServerInfoResponse{
		PackageName: s.meta.PackageName,
		Version:     s.meta.Version,
		RepoUrl:     s.meta.RepoURL,
	}, nil
}

func (s *Service) ListDefaults(ctx context.Context, req *tmuxproto.ListDefaultsRequest) (*tmuxproto.ListDefaultsResponse, error) {
	resp := &tmuxproto.ListDefaultsResponse{}
	if s.defaultTarget != nil {
		resp.CurrentDefault = s.defaultTarget
		resp.FromDisk = s.defaultsPath != ""
	}
	return resp, nil
}

func (s *Service) ValidateHost(ctx context.Context, req *tmuxproto.ValidateHostRequest) (*tmuxproto.ValidateHostResponse, error) {
	h := req.GetHost()
	if h == "" {
		return &tmuxproto.ValidateHostResponse{Found: false}, nil
	}
	p, ok := s.hostProfiles[h]
	resp := &tmuxproto.ValidateHostResponse{Found: ok}
	if ok {
		resp.TmuxBin = p.TmuxBin
		resp.PathAdd = p.PathAdd
		resp.Defaults = &tmuxproto.PaneRef{Host: h, Session: p.DefaultSession, Pane: p.DefaultPane}
	}
	return resp, nil
}

func (s *Service) ListSessions(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	out, err := s.runTmux(ctx, target.Host, []string{"list-sessions"})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListWindows(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-windows"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := s.runTmux(ctx, target.Host, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) ListPanes(ctx context.Context, req *tmuxproto.ListRequest) (*tmuxproto.ListResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	args := []string{"list-panes"}
	if target.Session != "" {
		args = append(args, "-t", target.Session)
	}
	out, err := s.runTmux(ctx, target.Host, args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &tmuxproto.ListResponse{Text: out}, nil
}

func (s *Service) SetDefault(ctx context.Context, req *tmuxproto.SetDefaultRequest) (*tmuxproto.SetDefaultResponse, error) {
	target, err := s.requireTarget(req.GetTarget())
	if err != nil {
		return nil, err
	}
	s.defaultTarget = target
	persistDefaultTarget(s.defaultsPath, target)
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

func isDestructive(args []string) bool {
	if len(args) == 0 {
		return false
	}
	destructive := map[string]bool{
		"kill-session":  true,
		"kill-window":   true,
		"kill-pane":     true,
		"kill-server":   true,
		"unlink-window": true,
		"unlink-pane":   true,
	}
	verb := args[0]
	if destructive[verb] {
		return true
	}
	if verb == "attach-session" {
		for _, a := range args {
			if a == "-k" {
				return true
			}
		}
	}
	return false
}

func (s *Service) streamViaPipe(ctx context.Context, stream tmuxproto.TmuxService_StreamPaneServer, target *tmuxproto.PaneRef, pane string, strip bool, maxBytes uint32, interval time.Duration, startSeq uint64) error {
	pipeDir := fmt.Sprintf("/tmp/mcp-tmux-%d-%d", time.Now().UnixNano(), rand.Intn(10000))
	pipePath := filepath.Join(pipeDir, "pipe")
	cleanup := func() {
		if target.Host != "" {
			_, _ = s.runTmux(context.Background(), target.Host, []string{"run-shell", fmt.Sprintf("rm -rf %s", pipeDir)})
		} else {
			_ = os.RemoveAll(pipeDir)
		}
	}

	var reader io.ReadCloser

	if target.Host == "" {
		if err := os.MkdirAll(pipeDir, 0700); err != nil {
			return err
		}
		if err := syscallMkfifo(pipePath, 0600); err != nil {
			_ = os.RemoveAll(pipeDir)
			return err
		}
		startArgs := []string{"pipe-pane", "-t", pane, fmt.Sprintf("cat >> %s", pipePath)}
		if _, err := s.runTmux(ctx, target.Host, startArgs); err != nil {
			_ = os.RemoveAll(pipeDir)
			return err
		}
		f, err := os.Open(pipePath)
		if err != nil {
			_ = os.RemoveAll(pipeDir)
			return err
		}
		reader = f
	} else {
		start := []string{
			"run-shell",
			fmt.Sprintf("mkdir -p %s && rm -f %s && mkfifo %s", pipeDir, pipePath, pipePath),
		}
		if _, err := s.runTmux(ctx, target.Host, start); err != nil {
			return err
		}
		pipeCmd := fmt.Sprintf("cat >> %s", pipePath)
		if _, err := s.runTmux(ctx, target.Host, []string{"pipe-pane", "-t", pane, pipeCmd}); err != nil {
			cleanup()
			return err
		}
		sshCmd := exec.CommandContext(ctx, "ssh", "-T", target.Host, "cat", pipePath)
		stdout, err := sshCmd.StdoutPipe()
		if err != nil {
			cleanup()
			return err
		}
		if err := sshCmd.Start(); err != nil {
			cleanup()
			return err
		}
		reader = stdout
		go func() {
			<-ctx.Done()
			_ = sshCmd.Process.Kill()
		}()
	}
	defer cleanup()

	defer s.runTmux(context.Background(), target.Host, []string{"pipe-pane", "-t", pane})

	bufReader := bufio.NewReader(reader)

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
			n, readErr := bufReader.Read(buf)
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

func (s *Service) requireTarget(target *tmuxproto.PaneRef) (*tmuxproto.PaneRef, error) {
	if target == nil {
		if s.defaultTarget != nil {
			return s.defaultTarget, nil
		}
		return nil, status.Error(codes.InvalidArgument, "target required")
	}
	return target, nil
}

func (s *Service) resolvePaneTarget(target *tmuxproto.PaneRef) (*tmuxproto.PaneRef, string, error) {
	target, err := s.requireTarget(target)
	if err != nil {
		return nil, "", err
	}
	// clone to avoid mutating caller
	t := *target
	target = &t
	if hp, ok := s.hostProfiles[target.Host]; ok {
		if target.Session == "" && hp.DefaultSession != "" {
			target.Session = hp.DefaultSession
		}
		if target.Pane == "" && hp.DefaultPane != "" {
			target.Pane = hp.DefaultPane
		}
	}
	pane := target.Pane
	if pane == "" && target.Window != "" && target.Session != "" {
		pane = fmt.Sprintf("%s:%s.0", target.Session, target.Window)
	}
	if pane == "" && target.Session != "" {
		pane = fmt.Sprintf("%s.0", target.Session)
	}
	if pane == "" {
		return nil, "", status.Error(codes.InvalidArgument, "pane required (set defaults or provide pane/session/window)")
	}
	return target, pane, nil
}
