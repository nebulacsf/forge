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
	Model            string  `json:"model"`
	Mode             string  `json:"mode"`
	MaxFileReadChars int     `json:"max_file_read_chars"`
	BashTimeoutSec   int     `json:"bash_timeout_sec"`
	MaxToolRounds    int     `json:"max_tool_rounds"`
	MaxTurnRounds    int     `json:"max_turn_rounds"`
	AutoContinue     *bool   `json:"auto_continue"`
	MaxTurnCostUSD   float64 `json:"max_turn_cost_usd"`
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
			Model:            DefaultModel,
			Mode:             "PLAN",
			MaxFileReadChars: 25000,
			BashTimeoutSec:   30,
			MaxToolRounds:    24,
			MaxTurnRounds:    96,
			AutoContinue:     boolPtr(true),
			MaxTurnCostUSD:   0,
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
	if c.Defaults.AutoContinue == nil {
		c.Defaults.AutoContinue = d.Defaults.AutoContinue
	}
	if c.Defaults.MaxTurnCostUSD < 0 {
		c.Defaults.MaxTurnCostUSD = 0
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
