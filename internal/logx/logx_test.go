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
		applyLLM(false)
		forcedLLM.Store(false)
		forcedDebug.Store(false)
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

func TestLLMTraceLevelAndDump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.log")
	resetLogger(t)
	if err := Init(Options{FilePath: path, LLMDebug: true}); err != nil {
		t.Fatal(err)
	}
	if !LLMEnabled() {
		t.Fatal("LLMEnabled() = false while tracing is on")
	}
	if !DebugEnabled() {
		t.Error("DebugEnabled() = false while tracing is on; trace must imply debug")
	}
	LLMTrace("llm request", "call", "call-1")
	LLMDump("llm request call-1", "{\n  \"model\": \"m\",\n  \"content\": \"hi\"\n}")
	if err := Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"level=LLM", "msg=\"llm request call-1\"", "\"model\": \"m\"", "\"content\": \"hi\""} {
		if !strings.Contains(text, want) {
			t.Errorf("log missing %q:\n%s", want, text)
		}
	}
	// The dump must not be escaped by the text handler.
	if strings.Contains(text, `\n  \"model\"`) {
		t.Errorf("dump was escaped instead of written verbatim:\n%s", text)
	}

	// Disabling tracing must silence both the level and the dumps.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Init(Options{FilePath: path, LLMDebug: false}); err != nil {
		t.Fatal(err)
	}
	if LLMEnabled() {
		t.Error("LLMEnabled() = true after tracing was disabled")
	}
	LLMTrace("hidden trace")
	LLMDump("hidden dump", "{\"secret\": true}")
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hidden trace") || strings.Contains(string(data), "hidden dump") {
		t.Errorf("trace records written while tracing was disabled:\n%s", data)
	}
}

func TestForcedLLMSurvivesConfigReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.log")
	resetLogger(t)
	if err := Init(Options{FilePath: path, LLMDebug: true, ForceLLM: true}); err != nil {
		t.Fatal(err)
	}
	ApplyLLM(false)
	ApplyDebug(false)
	if !LLMEnabled() {
		t.Fatal("forced trace mode was downgraded by a config reload")
	}
	if !DebugEnabled() {
		t.Error("forced trace mode must keep DEBUG enabled")
	}
	if err := Close(); err != nil {
		t.Fatal(err)
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
