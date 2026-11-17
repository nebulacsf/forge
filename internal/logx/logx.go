// Package logx provides leveled logging to stderr and a rotating local log
// file. Debug logging is opt-in and never records API keys or full prompts.
//
// A second, lower tier (LLM) is available through --debug-llm. It dumps the
// complete provider payloads verbatim: every message sent to the model, the
// raw stream chunks, the assembled response and provider error bodies. API keys
// and the Authorization header are never written at any level.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LevelLLM is the trace level used by --debug-llm. It sits below DEBUG so
// enabling it also enables every DEBUG record.
const LevelLLM = slog.LevelDebug - 4

// Options configures the process logger.
type Options struct {
	// Debug enables DEBUG output. ForceDebug keeps DEBUG enabled even if the
	// configuration later turns it off (used for --debug / FORGE_DEBUG).
	Debug      bool
	ForceDebug bool
	// LLMDebug enables the LLM trace level below DEBUG. ForceLLM keeps it on
	// even if the configuration later turns it off (--debug-llm /
	// FORGE_LLM_DEBUG).
	LLMDebug bool
	ForceLLM bool
	// FilePath is the log file to append to. Empty disables file logging.
	FilePath string
	// RotateBytes rotates the log file once it grows past this size.
	RotateBytes int64
}

const defaultRotateBytes = 10 << 20 // 10 MiB

var (
	level       = new(slog.LevelVar)
	logger      atomic.Pointer[slog.Logger]
	debugWanted atomic.Bool
	forcedDebug atomic.Bool
	llmWanted   atomic.Bool
	forcedLLM   atomic.Bool
	fileSink              = &rotatingSink{}
	sink        io.Writer = io.MultiWriter(os.Stderr, fileSink)
	dumpMu      sync.Mutex
)

func init() {
	logger.Store(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})))
	level.Set(slog.LevelInfo)
}

// replaceAttr renames the custom trace level so records read "level=LLM"
// instead of the raw numeric value.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key != slog.LevelKey {
		return a
	}
	if l, ok := a.Value.Any().(slog.Level); ok && l <= LevelLLM {
		a.Value = slog.StringValue("LLM")
	}
	return a
}

// Init configures the global logger. It is safe to call more than once.
func Init(o Options) error {
	if o.ForceDebug {
		forcedDebug.Store(true)
	}
	if o.ForceLLM {
		forcedLLM.Store(true)
	}
	applyDebug(o.Debug)
	applyLLM(o.LLMDebug)
	if o.FilePath == "" {
		return nil
	}
	return fileSink.open(o.FilePath, o.RotateBytes)
}

// ApplyDebug updates the level at runtime, respecting an explicitly forced
// debug mode from the command line or environment.
func ApplyDebug(enabled bool) {
	applyDebug(enabled)
}

// ApplyLLM updates the LLM trace switch at runtime, respecting an explicitly
// forced trace mode from the command line or environment.
func ApplyLLM(enabled bool) {
	applyLLM(enabled)
}

// DebugEnabled reports whether DEBUG output is currently active. The LLM trace
// level implies DEBUG.
func DebugEnabled() bool {
	return level.Level() <= slog.LevelDebug
}

// LLMEnabled reports whether full LLM payload tracing is active.
func LLMEnabled() bool {
	return level.Level() <= LevelLLM
}

// Close flushes and closes the log file.
func Close() error {
	return fileSink.close()
}

// Logger returns the underlying slog logger.
func Logger() *slog.Logger {
	return logger.Load()
}

func Debug(msg string, args ...any) { write(slog.LevelDebug, msg, args...) }
func Info(msg string, args ...any)  { write(slog.LevelInfo, msg, args...) }
func Warn(msg string, args ...any)  { write(slog.LevelWarn, msg, args...) }
func Error(msg string, args ...any) { write(slog.LevelError, msg, args...) }

// LLMTrace writes one trace record at the LLM level. Short values (ids, sizes,
// single stream chunks) belong here; whole payloads should use LLMDump.
func LLMTrace(msg string, args ...any) { write(LevelLLM, msg, args...) }

// LLMDump writes a raw, multi-line block at the LLM level. It bypasses the slog
// text handler on purpose: the handler would escape newlines and quotes, which
// makes JSON payloads unreadable.
func LLMDump(label, text string) {
	if !LLMEnabled() {
		return
	}
	text = strings.TrimRight(text, "\n")
	var b strings.Builder
	b.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
	b.WriteString(" level=LLM msg=")
	b.WriteString(strconv.Quote(label))
	b.WriteString("\n")
	if text == "" {
		text = "(empty)"
	}
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	dumpMu.Lock()
	defer dumpMu.Unlock()
	_, _ = io.WriteString(sink, b.String())
}

// Fatalf logs at ERROR level and exits with status 1.
func Fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	write(slog.LevelError, msg)
	_ = Close()
	os.Exit(1)
}

func write(l slog.Level, msg string, args ...any) {
	if l < level.Level() {
		return
	}
	logger.Load().Log(context.Background(), l, msg, args...)
}

func applyDebug(enabled bool) {
	debugWanted.Store(enabled)
	applyLevels()
}

func applyLLM(enabled bool) {
	llmWanted.Store(enabled)
	applyLevels()
}

func applyLevels() {
	switch {
	case forcedLLM.Load() || llmWanted.Load():
		level.Set(LevelLLM)
	case forcedDebug.Load() || debugWanted.Load():
		level.Set(slog.LevelDebug)
	default:
		level.Set(slog.LevelInfo)
	}
}

// rotatingSink is an io.Writer that appends to the current log file.
type rotatingSink struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func (s *rotatingSink) open(path string, rotateBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rotateBytes <= 0 {
		rotateBytes = defaultRotateBytes
	}
	if s.file != nil && s.path == path {
		return nil
	}
	s.closeLocked()
	if info, err := os.Stat(path); err == nil && info.Size() >= rotateBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	s.file = f
	s.path = path
	return nil
}

func (s *rotatingSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *rotatingSink) closeLocked() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	s.path = ""
	return err
}

func (s *rotatingSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return len(p), nil
	}
	if _, err := s.file.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator {
			if i == 0 {
				return string(os.PathSeparator)
			}
			return path[:i]
		}
	}
	return ""
}

// Preview shortens text for log output.
func Preview(text string, limit int) string {
	if limit <= 0 {
		limit = 160
	}
	out := make([]rune, 0, limit+1)
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if len(out) >= limit {
			return string(out) + "…"
		}
		out = append(out, r)
	}
	return string(out)
}
