// Package logx provides leveled logging to stderr and a rotating local log
// file. Debug logging is opt-in and never records API keys or full prompts.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// Options configures the process logger.
type Options struct {
	// Debug enables DEBUG output. ForceDebug keeps DEBUG enabled even if the
	// configuration later turns it off (used for --debug / FORGE_DEBUG).
	Debug      bool
	ForceDebug bool
	// FilePath is the log file to append to. Empty disables file logging.
	FilePath string
	// RotateBytes rotates the log file once it grows past this size.
	RotateBytes int64
}

const defaultRotateBytes = 10 << 20 // 10 MiB

var (
	level       = new(slog.LevelVar)
	logger      atomic.Pointer[slog.Logger]
	forcedDebug atomic.Bool
	fileSink    = &rotatingSink{}
)

func init() {
	logger.Store(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, fileSink), &slog.HandlerOptions{
		Level: level,
	})))
	level.Set(slog.LevelInfo)
}

// Init configures the global logger. It is safe to call more than once.
func Init(o Options) error {
	if o.ForceDebug {
		forcedDebug.Store(true)
	}
	applyDebug(o.Debug)
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

// DebugEnabled reports whether DEBUG output is currently active.
func DebugEnabled() bool {
	return level.Level() <= slog.LevelDebug
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
	if enabled || forcedDebug.Load() {
		level.Set(slog.LevelDebug)
		return
	}
	level.Set(slog.LevelInfo)
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
