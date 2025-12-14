package tmux

import "testing"

func TestBuildPathDedup(t *testing.T) {
	got := BuildPath("/usr/bin:/bin", []string{"/opt/bin", "/usr/bin", "/custom"})
	want := "/usr/bin:/bin:/opt/bin:/custom"
	if got != want {
		t.Fatalf("BuildPath mismatch: got %q want %q", got, want)
	}
}
