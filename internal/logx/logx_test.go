package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = Close()
		applyDebug(false)
	})
}

func TestFileLoggingAndLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "forge.log")
	resetLogger(t)
	if err := Init(Options{Debug: false, FilePath: path}); err != nil {
		t.Fatal(err)
	}
	Debug("hidden debug line")
	Info("visible info line", "key", "value")
	Warn("visible warning")
	Error("visible error")
	if err := Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "hidden debug line") {
		t.Error("debug line was written while debug was disabled")
	}
	for _, want := range []string{"visible info line", "visible warning", "visible error", "level=INFO", "level=WARN", "level=ERROR"} {
		if !strings.Contains(text, want) {
			t.Errorf("log file missing %q:\n%s", want, text)
		}
	}
}

func TestDebugLoggingAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.log")
	resetLogger(t)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(Options{Debug: true, FilePath: path, RotateBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	Debug("verbose detail", "session", 7)
	if err := Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated log backup: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "level=DEBUG") || !strings.Contains(string(data), "verbose detail") {
		t.Errorf("debug line missing from log:\n%s", data)
	}
	if !DebugEnabled() {
		t.Error("DebugEnabled() = false while debug is on")
	}
}

func TestPreview(t *testing.T) {
	got := Preview("hello\nworld\tagain", 8)
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("preview contains control characters: %q", got)
	}
	if len([]rune(got)) != 9 { // 8 runes + ellipsis
		t.Errorf("preview length = %d, want 9 (%q)", len([]rune(got)), got)
	}
}
