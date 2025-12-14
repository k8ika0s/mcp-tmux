package server

import (
	"context"
	"testing"
	"time"

	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeRunner struct {
	calls   [][]string
	outputs []string
	err     error
}

func (f *fakeRunner) run(ctx context.Context, host, tmuxBin string, pathAdd []string, args []string) (string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", f.err
	}
	if len(f.outputs) == 0 {
		return "", nil
	}
	out := f.outputs[0]
	if len(f.outputs) > 1 {
		f.outputs = f.outputs[1:]
	}
	return out, nil
}

func TestSendKeysValidations(t *testing.T) {
	svc := NewServiceWithRunner("tmux", nil, (&fakeRunner{}).run)
	_, err := svc.SendKeys(context.Background(), &tmuxproto.SendKeysRequest{
		Target: &tmuxproto.PaneRef{Session: "s"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}

func TestSendKeysEnterOnly(t *testing.T) {
	r := &fakeRunner{}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	resp, err := svc.SendKeys(context.Background(), &tmuxproto.SendKeysRequest{
		Target: &tmuxproto.PaneRef{Session: "s"},
		Enter:  true,
	})
	if err != nil {
		t.Fatalf("SendKeys error: %v", err)
	}
	if resp.Text == "" {
		t.Fatalf("expected response text")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(r.calls))
	}
	want := []string{"send-keys", "-t", "s.0", "Enter"}
	if !equalStrings(r.calls[0], want) {
		t.Fatalf("args mismatch: got %v want %v", r.calls[0], want)
	}
}

func TestRunBatchJoinAndClean(t *testing.T) {
	r := &fakeRunner{}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	_, err := svc.RunBatch(context.Background(), &tmuxproto.RunBatchRequest{
		Target:      &tmuxproto.PaneRef{Session: "s"},
		Steps:       []string{"echo 1", "echo 2"},
		CleanPrompt: true,
	})
	if err != nil {
		t.Fatalf("RunBatch error: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 calls (clean + send), got %d", len(r.calls))
	}
	if !equalStrings(r.calls[0], []string{"send-keys", "-t", "s.0", "C-c", "C-u"}) {
		t.Fatalf("clean args mismatch: %v", r.calls[0])
	}
	if !equalStrings(r.calls[1], []string{"send-keys", "-t", "s.0", "echo 1 && echo 2", "Enter"}) {
		t.Fatalf("batch args mismatch: %v", r.calls[1])
	}
}

func TestRunBatchCaptureTruncated(t *testing.T) {
	r := &fakeRunner{outputs: []string{"ok", "line1\nline2\nline3"}}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	resp, err := svc.RunBatch(context.Background(), &tmuxproto.RunBatchRequest{
		Target:       &tmuxproto.PaneRef{Session: "s"},
		Steps:        []string{"echo hi"},
		CaptureLines: 2,
	})
	if err != nil {
		t.Fatalf("RunBatch error: %v", err)
	}
	if !resp.Truncated {
		t.Fatalf("expected truncated capture")
	}
}

func TestRunCommandStripANSI(t *testing.T) {
	r := &fakeRunner{outputs: []string{"\x1b[31mred\x1b[0m"}}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	resp, err := svc.RunCommand(context.Background(), &tmuxproto.RunCommandRequest{
		Target:    &tmuxproto.PaneRef{Session: "s"},
		Args:      []string{"display-message"},
		StripAnsi: true,
	})
	if err != nil {
		t.Fatalf("RunCommand error: %v", err)
	}
	if resp.Text != "red" {
		t.Fatalf("expected stripped output, got %q", resp.Text)
	}
}

func TestMultiRunAggregates(t *testing.T) {
	r := &fakeRunner{outputs: []string{"ok"}}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	resp, err := svc.MultiRun(context.Background(), &tmuxproto.MultiRunRequest{
		Steps: []*tmuxproto.MultiRunStep{
			{Target: &tmuxproto.PaneRef{Session: "s"}, Args: []string{"list-sessions"}},
			{Target: &tmuxproto.PaneRef{Session: "s2"}},
		},
	})
	if err != nil {
		t.Fatalf("MultiRun error: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[1].Error == "" {
		t.Fatalf("expected error for missing args")
	}
}

func TestCaptureAndRestoreLayout(t *testing.T) {
	r := &fakeRunner{outputs: []string{"@1\tdeadbeef\n@2\tcafebabe"}}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	capResp, err := svc.CaptureLayout(context.Background(), &tmuxproto.CaptureLayoutRequest{
		Target: &tmuxproto.PaneRef{Session: "s"},
	})
	if err != nil {
		t.Fatalf("CaptureLayout error: %v", err)
	}
	if len(capResp.Layouts) != 2 {
		t.Fatalf("expected 2 layouts, got %d", len(capResp.Layouts))
	}

	restoreRunner := &fakeRunner{}
	svc2 := NewServiceWithRunner("tmux", nil, restoreRunner.run)
	_, err = svc2.RestoreLayout(context.Background(), &tmuxproto.RestoreLayoutRequest{
		Target:  &tmuxproto.PaneRef{Session: "s"},
		Layouts: capResp.Layouts,
	})
	if err != nil {
		t.Fatalf("RestoreLayout error: %v", err)
	}
	if len(restoreRunner.calls) != 2 {
		t.Fatalf("expected 2 layout apply calls, got %d", len(restoreRunner.calls))
	}
}

func TestNewSessionAndWindow(t *testing.T) {
	r := &fakeRunner{}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	if _, err := svc.NewSession(context.Background(), &tmuxproto.NewSessionRequest{
		Target:  &tmuxproto.PaneRef{Session: "s"},
		Command: "echo hi",
	}); err != nil {
		t.Fatalf("NewSession error: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0][0] != "new-session" {
		t.Fatalf("unexpected new-session call: %v", r.calls)
	}

	r2 := &fakeRunner{}
	svc2 := NewServiceWithRunner("tmux", nil, r2.run)
	if _, err := svc2.NewWindow(context.Background(), &tmuxproto.NewWindowRequest{
		Target:  &tmuxproto.PaneRef{Session: "s"},
		Name:    "win",
		Command: "pwd",
	}); err != nil {
		t.Fatalf("NewWindow error: %v", err)
	}
	if len(r2.calls) != 1 || r2.calls[0][0] != "new-window" {
		t.Fatalf("unexpected new-window call: %v", r2.calls)
	}
}

type stubStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	msgs   []*tmuxproto.PaneChunk
}

func (s *stubStream) Send(chunk *tmuxproto.PaneChunk) error {
	s.msgs = append(s.msgs, chunk)
	if len(s.msgs) >= 2 && s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *stubStream) Context() context.Context { return s.ctx }

// Unused methods to satisfy interface.
func (*stubStream) SetHeader(metadata.MD) error  { return nil }
func (*stubStream) SendHeader(metadata.MD) error { return nil }
func (*stubStream) SetTrailer(metadata.MD)       {}
func (*stubStream) SendMsg(m interface{}) error  { _ = m; return nil }
func (*stubStream) RecvMsg(m interface{}) error  { _ = m; return nil }

func TestStreamPaneDelta(t *testing.T) {
	prevPoll, prevHeartbeat := pollInterval, heartbeatInterval
	pollInterval = 10 * time.Millisecond
	heartbeatInterval = time.Hour
	defer func() {
		pollInterval = prevPoll
		heartbeatInterval = prevHeartbeat
	}()

	r := &fakeRunner{outputs: []string{"", "foo", "foobar"}}
	svc := NewServiceWithRunner("tmux", nil, r.run)
	ctx, cancel := context.WithCancel(context.Background())
	stream := &stubStream{ctx: ctx, cancel: cancel}
	err := svc.StreamPane(&tmuxproto.StreamPaneRequest{
		Target:     &tmuxproto.PaneRef{Session: "s"},
		PollMillis: 10,
	}, stream)
	if err != nil {
		t.Fatalf("StreamPane error: %v", err)
	}
	if len(stream.msgs) < 2 {
		t.Fatalf("expected deltas sent, got %d", len(stream.msgs))
	}
	if string(stream.msgs[0].Data) != "foo" || string(stream.msgs[1].Data) != "bar" {
		t.Fatalf("unexpected deltas: %q %q", stream.msgs[0].Data, stream.msgs[1].Data)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
