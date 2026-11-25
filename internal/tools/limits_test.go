package tools

import (
	"strings"
	"testing"
	"time"
)

// A configured 0 means "no limit", so read_file must return the whole requested
// window instead of falling back to the shipped 25,000-character ceiling.
func TestReadFileWithoutCharacterLimit(t *testing.T) {
	m, _ := newTestManager(t, "PLAN", 0)
	res := run(t, m, "read_file", map[string]any{"path": "hello.go", "start_line": 1, "end_line": 4})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Greeting() string") {
		t.Errorf("whole file should be returned without a cap: %q", res.Content)
	}
	if strings.Contains(res.Content, "[output truncated") {
		t.Errorf("no character cap was configured, got truncation notice: %q", res.Content)
	}
}

// The line window is the shape of the tool, not a setting: an unbounded read
// still caps at maxReadLines so one call cannot return a whole large file.
func TestReadFileKeepsLineWindowWithoutCharacterLimit(t *testing.T) {
	m, _ := newTestManager(t, "PLAN", 0)
	res := run(t, m, "read_file", map[string]any{"path": "hello.go", "start_line": 1, "end_line": 500})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "lines 1-4 of 4") {
		t.Errorf("expected the file's own line range to bound the read: %q", res.Content)
	}
}

func TestNewManagerKeepsZeroLimits(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, 0, 0, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxRead != 0 {
		t.Errorf("MaxRead = %d, want 0 (no limit)", m.MaxRead)
	}
	if m.BashTimeout != 0 {
		t.Errorf("BashTimeout = %v, want 0 (no timeout)", m.BashTimeout)
	}

	// A negative value is the legacy "not configured" signal and still falls
	// back to the shipped defaults.
	m, err = NewManager(root, -1, -1, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxRead != readOutputLimit {
		t.Errorf("MaxRead = %d, want %d for a negative value", m.MaxRead, readOutputLimit)
	}
	if m.BashTimeout != 30*time.Second {
		t.Errorf("BashTimeout = %v, want 30s for a negative value", m.BashTimeout)
	}
}

func TestResolveBashTimeout(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		timeoutSec int
		want       time.Duration
	}{
		{"configured default", 30 * time.Second, 0, 30 * time.Second},
		{"per-call overrides", 30 * time.Second, 5, 5 * time.Second},
		{"no timeout configured", 0, 0, 0},
		{"no timeout configured, per-call wins", 0, 90, 90 * time.Second},
		{"no upper ceiling", 0, 3600, 3600 * time.Second},
		{"configured ceiling is not clamped", 7200 * time.Second, 0, 7200 * time.Second},
		{"sub-second floor", 500 * time.Millisecond, 0, time.Second},
		{"negative per-call keeps the default", 30 * time.Second, -4, 30 * time.Second},
	}
	for _, c := range cases {
		if got := resolveBashTimeout(c.configured, c.timeoutSec); got != c.want {
			t.Errorf("%s: resolveBashTimeout(%v, %d) = %v, want %v",
				c.name, c.configured, c.timeoutSec, got, c.want)
		}
	}
}

// A zero timeout must not install a deadline: the command runs to completion and
// the result must not be flagged as a timeout.
func TestBashRunsWithoutTimeout(t *testing.T) {
	m, _ := newTestManager(t, "BUILD", 25000)
	m.BashTimeout = 0
	res := run(t, m, "bash", map[string]any{"command": "echo unlimited"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "unlimited") {
		t.Errorf("missing command output: %q", res.Content)
	}
	if strings.Contains(res.Content, "timed out") {
		t.Errorf("no timeout was configured, got a timeout notice: %q", res.Content)
	}
}
