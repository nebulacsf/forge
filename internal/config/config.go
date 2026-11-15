// Package config manages Forge's on-disk configuration.
//
// The configuration lives in ${FORGE_HOME:-$HOME/.forge}/config.json. It is
// intentionally small, human editable, and safe to reload from the UI.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Default model used for brand new sessions.
const DefaultModel = "qwen/qwen-2.5-coder-32b-instruct"

// Config is the complete persisted configuration.
type Config struct {
	Server    ServerConfig    `json:"server"`
	Providers ProvidersConfig `json:"providers"`
	Defaults  DefaultsConfig  `json:"defaults"`
}

type ServerConfig struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	AutoOpenBrowser bool   `json:"auto_open_browser"`
	Debug           bool   `json:"debug"`
	LogFile         string `json:"log_file"`
}

type ProvidersConfig struct {
	OpenRouter OpenRouterConfig `json:"openrouter"`
}

type OpenRouterConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

type DefaultsConfig struct {
	Model            string `json:"model"`
	Mode             string `json:"mode"`
	MaxFileReadChars int    `json:"max_file_read_chars"`
	BashTimeoutSec   int    `json:"bash_timeout_sec"`
	MaxToolRounds    int    `json:"max_tool_rounds"`
	MaxTurnRounds    int    `json:"max_turn_rounds"`
	// PLAN mode has its own research budget. When either limit is reached the
	// harness disables tools and requires the final plan, so a tool-hungry model
	// cannot loop indefinitely while researching.
	PlanRounds     int     `json:"plan_rounds"`
	PlanToolCalls  int     `json:"plan_tool_calls"`
	AutoContinue   *bool   `json:"auto_continue"`
	MaxTurnCostUSD float64 `json:"max_turn_cost_usd"`

	// History pruning keeps recent tool output verbatim and stubs older rounds
	// in place before each provider request. Volatile state (checklist, outline)
	// lives in a trailing message so the stable prompt prefix stays cacheable.
	HistoryKeepRecent int `json:"history_keep_recent_rounds"`
	HistoryMaxResult  int `json:"history_max_result_chars"`
	HistoryMaxArg     int `json:"history_max_arg_chars"`
	HistoryPruneChunk int `json:"history_prune_chunk_rounds"`
}

// Default returns the shipped configuration.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host:            "127.0.0.1",
			Port:            7890,
			AutoOpenBrowser: true,
		},
		Providers: ProvidersConfig{
			OpenRouter: OpenRouterConfig{
				APIKey:  "",
				BaseURL: "https://openrouter.ai/api/v1",
			},
		},
		Defaults: DefaultsConfig{
			Model:             DefaultModel,
			Mode:              "PLAN",
			MaxFileReadChars:  25000,
			BashTimeoutSec:    30,
			MaxToolRounds:     16,
			MaxTurnRounds:     64,
			PlanRounds:        16,
			PlanToolCalls:     32,
			AutoContinue:      boolPtr(true),
			MaxTurnCostUSD:    0,
			HistoryKeepRecent: 12,
			HistoryMaxResult:  8000,
			HistoryMaxArg:     4000,
			HistoryPruneChunk: 8,
		},
	}
}

// Manager owns the config file and provides concurrency safe access.
type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Dir returns the Forge home directory.
func Dir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("FORGE_HOME")); dir != "" {
		return filepath.Abs(expandHome(dir))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".forge"), nil
}

// DBPath returns the SQLite database path.
func DBPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "forge.db"), nil
}

// LogPath resolves the configured log file, defaulting to <forge home>/forge.log.
func LogPath(cfg Config) (string, error) {
	if p := strings.TrimSpace(cfg.Server.LogFile); p != "" {
		return filepath.Abs(expandHome(p))
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "forge.log"), nil
}

// Load opens (creating when missing) the configuration file.
func Load() (*Manager, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	path := filepath.Join(dir, "config.json")

	m := &Manager{path: path, cfg: Default()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := m.writeLocked(); err != nil {
			return nil, err
		}
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	}

	loaded := Default()
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m.cfg = normalize(loaded)
	return m, nil
}

// Path returns the absolute config file path.
func (m *Manager) Path() string { return m.path }

// Get returns a copy of the current configuration.
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update validates, persists, and applies a configuration.
func (m *Manager) Update(next Config) error {
	next = normalize(next)
	if err := validate(next); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.cfg
	m.cfg = next
	if err := m.writeLocked(); err != nil {
		m.cfg = prev
		return err
	}
	return nil
}

func (m *Manager) writeLocked() error {
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}

func normalize(c Config) Config {
	d := Default()
	if strings.TrimSpace(c.Server.Host) == "" {
		c.Server.Host = d.Server.Host
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		c.Server.Port = d.Server.Port
	}
	if strings.TrimSpace(c.Providers.OpenRouter.BaseURL) == "" {
		c.Providers.OpenRouter.BaseURL = d.Providers.OpenRouter.BaseURL
	}
	c.Providers.OpenRouter.BaseURL = strings.TrimRight(strings.TrimSpace(c.Providers.OpenRouter.BaseURL), "/")
	c.Providers.OpenRouter.APIKey = strings.TrimSpace(c.Providers.OpenRouter.APIKey)
	c.Server.LogFile = strings.TrimSpace(c.Server.LogFile)

	if strings.TrimSpace(c.Defaults.Model) == "" {
		c.Defaults.Model = d.Defaults.Model
	}
	c.Defaults.Mode = strings.ToUpper(strings.TrimSpace(c.Defaults.Mode))
	if c.Defaults.Mode != "BUILD" {
		c.Defaults.Mode = "PLAN"
	}
	if c.Defaults.MaxFileReadChars <= 0 {
		c.Defaults.MaxFileReadChars = d.Defaults.MaxFileReadChars
	}
	if c.Defaults.MaxFileReadChars > 2_000_000 {
		c.Defaults.MaxFileReadChars = 2_000_000
	}
	if c.Defaults.BashTimeoutSec <= 0 {
		c.Defaults.BashTimeoutSec = d.Defaults.BashTimeoutSec
	}
	if c.Defaults.BashTimeoutSec > 600 {
		c.Defaults.BashTimeoutSec = 600
	}
	if c.Defaults.MaxToolRounds <= 0 {
		c.Defaults.MaxToolRounds = d.Defaults.MaxToolRounds
	}
	if c.Defaults.MaxToolRounds > 200 {
		c.Defaults.MaxToolRounds = 200
	}
	if c.Defaults.MaxTurnRounds <= 0 {
		c.Defaults.MaxTurnRounds = d.Defaults.MaxTurnRounds
	}
	if c.Defaults.MaxTurnRounds > 1000 {
		c.Defaults.MaxTurnRounds = 1000
	}
	if c.Defaults.MaxTurnRounds < c.Defaults.MaxToolRounds {
		c.Defaults.MaxTurnRounds = c.Defaults.MaxToolRounds
	}
	if c.Defaults.PlanRounds <= 0 {
		c.Defaults.PlanRounds = d.Defaults.PlanRounds
	}
	if c.Defaults.PlanRounds > 24 {
		c.Defaults.PlanRounds = 24
	}
	if c.Defaults.PlanToolCalls <= 0 {
		c.Defaults.PlanToolCalls = d.Defaults.PlanToolCalls
	}
	if c.Defaults.PlanToolCalls > 200 {
		c.Defaults.PlanToolCalls = 200
	}
	if c.Defaults.AutoContinue == nil {
		c.Defaults.AutoContinue = d.Defaults.AutoContinue
	}
	if c.Defaults.MaxTurnCostUSD < 0 {
		c.Defaults.MaxTurnCostUSD = 0
	}
	if c.Defaults.HistoryKeepRecent <= 0 {
		c.Defaults.HistoryKeepRecent = d.Defaults.HistoryKeepRecent
	}
	if c.Defaults.HistoryKeepRecent > 20 {
		c.Defaults.HistoryKeepRecent = 20
	}
	// 0 disables the cap; negative values fall back to the shipped default.
	if c.Defaults.HistoryMaxResult < 0 {
		c.Defaults.HistoryMaxResult = d.Defaults.HistoryMaxResult
	}
	if c.Defaults.HistoryMaxResult > 0 && c.Defaults.HistoryMaxResult < 500 {
		c.Defaults.HistoryMaxResult = 500
	}
	if c.Defaults.HistoryMaxResult > 25000 {
		c.Defaults.HistoryMaxResult = 25000
	}
	if c.Defaults.HistoryMaxArg < 0 {
		c.Defaults.HistoryMaxArg = d.Defaults.HistoryMaxArg
	}
	if c.Defaults.HistoryMaxArg > 0 && c.Defaults.HistoryMaxArg < 200 {
		c.Defaults.HistoryMaxArg = 200
	}
	if c.Defaults.HistoryMaxArg > 25000 {
		c.Defaults.HistoryMaxArg = 25000
	}
	if c.Defaults.HistoryPruneChunk <= 0 {
		c.Defaults.HistoryPruneChunk = d.Defaults.HistoryPruneChunk
	}
	if c.Defaults.HistoryPruneChunk > 20 {
		c.Defaults.HistoryPruneChunk = 20
	}
	return c
}

func boolPtr(v bool) *bool { return &v }

func validate(c Config) error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if !strings.HasPrefix(c.Providers.OpenRouter.BaseURL, "http://") &&
		!strings.HasPrefix(c.Providers.OpenRouter.BaseURL, "https://") {
		return fmt.Errorf("OpenRouter base URL must start with http:// or https://")
	}
	if strings.TrimSpace(c.Defaults.Model) == "" {
		return fmt.Errorf("default model cannot be empty")
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
