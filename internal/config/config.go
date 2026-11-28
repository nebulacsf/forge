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
	// DebugLLM enables the extra-verbose LLM trace level (--debug-llm): every
	// request and response payload sent to the model is written to the log.
	DebugLLM bool   `json:"debug_llm"`
	LogFile  string `json:"log_file"`
}

type ProvidersConfig struct {
	OpenRouter OpenRouterConfig `json:"openrouter"`
}

type OpenRouterConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

// DefaultsConfig holds the per-turn limits. They all share one contract: an
// explicit 0 means "no limit" and is honored as written, while a negative value
// means "not configured" and falls back to the shipped default. There is no
// upper bound: how far a turn may go is the developer's decision alone.
type DefaultsConfig struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
	// MaxFileReadChars caps the characters read_file returns per call.
	// 0 returns every requested line.
	MaxFileReadChars int `json:"max_file_read_chars"`
	// BashTimeoutSec caps a shell command. 0 means no timeout at all: the
	// command runs until it finishes or the turn is cancelled.
	BashTimeoutSec int `json:"bash_timeout_sec"`
	// MaxToolRounds is the checkpoint size of a BUILD turn. 0 runs the whole
	// turn as a single segment, so no checkpoint is ever reached.
	MaxToolRounds int `json:"max_tool_rounds"`
	// MaxTurnRounds bounds a whole turn, checkpoints included. 0 means the turn
	// only ends when the model stops calling tools or the user cancels.
	MaxTurnRounds int `json:"max_turn_rounds"`
	// PLAN mode has its own research budget. When either limit is reached the
	// harness disables tools and requires the final plan, so a tool-hungry model
	// cannot loop indefinitely while researching. Setting both to 0 removes the
	// budget: research is then bounded by the turn ceiling and the loop guard.
	PlanRounds     int     `json:"plan_rounds"`
	PlanToolCalls  int     `json:"plan_tool_calls"`
	AutoContinue   *bool   `json:"auto_continue"`
	MaxTurnCostUSD float64 `json:"max_turn_cost_usd"`
	// BuildVerifyRounds bounds the BUILD verification gate. When the model
	// tries to close a turn that wrote files without running any bash or
	// git_diff call since the last write, the harness rejects the close and
	// forces up to this many verification rounds; once they are spent the
	// turn may only close by disclosing that verification did not run. A
	// configured 0 disables the gate entirely.
	BuildVerifyRounds int `json:"build_verify_rounds"`
	// MaxConcurrentRuns is a global safety valve for parallel agents. It never
	// shortens a turn: it only rejects a new run while that many are already
	// executing. Within a project a BUILD run is always exclusive, while PLAN
	// runs (read-only) may be added alongside anything. 0 means unlimited.
	MaxConcurrentRuns int `json:"max_concurrent_runs"`

	// History pruning keeps recent tool output verbatim and stubs older rounds
	// in place before each provider request. Volatile state (checklist, outline)
	// lives in a trailing message so the stable prompt prefix stays cacheable.
	//
	// Pruning is budget driven by default: the conversation must fit in
	// min(model context length * HistoryPrunePercent, HistoryPruneMaxTokens)
	// tokens. The round-based knobs below stay available but are disabled (0)
	// unless the token budget itself is disabled.
	HistoryPrunePercent   int  `json:"history_prune_percent"`
	HistoryPruneMaxTokens int  `json:"history_prune_max_tokens"`
	HistoryKeepRecent     int  `json:"history_keep_recent_rounds"`
	HistoryMaxResult      int  `json:"history_max_result_chars"`
	HistoryMaxArg         int  `json:"history_max_arg_chars"`
	HistoryPruneChunk     int  `json:"history_prune_chunk_rounds"`
	BrowserNotifications  bool `json:"browser_notifications"`
	// OpenRouterCatalogFallback lets Forge read the public OpenRouter catalog to
	// fill in context windows the active provider does not publish. Providers
	// such as OpenCode Go report the window as `limit.context`; gateways that
	// report nothing at all end up here. It only ever supplies metadata.
	OpenRouterCatalogFallback *bool `json:"openrouter_catalog_fallback"`
}

// OpenRouterCatalogEnabled reports whether the OpenRouter catalog may be used to
// fill in missing context windows. A nil pointer means the shipped default (on).
func (d DefaultsConfig) OpenRouterCatalogEnabled() bool {
	return d.OpenRouterCatalogFallback == nil || *d.OpenRouterCatalogFallback
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
			MaxToolRounds:    16,
			MaxTurnRounds:    64,
			PlanRounds:       16,
			PlanToolCalls:    32,
			AutoContinue:     boolPtr(true),
			MaxTurnCostUSD:   0,
			// The BUILD verification gate ships on: two forced verification
			// rounds before an unverified close is accepted, then disclosure.
			BuildVerifyRounds: 2,
			// Parallel agents: several projects (and several PLAN chats) can
			// run at once; this only bounds the machine, never a single turn.
			MaxConcurrentRuns: 6,
			// Budget-driven pruning is the default: the request may use up to
			// 70% of the model's context window, capped at 120k tokens.
			HistoryPrunePercent:   70,
			HistoryPruneMaxTokens: 120000,
			// Round-based pruning stays available but is off by default.
			HistoryKeepRecent:    0,
			HistoryMaxResult:     16000,
			HistoryMaxArg:        4000,
			HistoryPruneChunk:    0,
			BrowserNotifications: false,
			// OpenRouter's public catalog is the only cross-provider source of
			// context windows Forge can consult, so it ships enabled.
			OpenRouterCatalogFallback: boolPtr(true),
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
	// Every limit shares one contract: a negative value means "not configured"
	// and falls back to the shipped default, while an explicit 0 means "no
	// limit" and is preserved verbatim. Nothing is clamped to a ceiling any
	// more, so how far a turn may go is the developer's decision alone.
	if c.Defaults.MaxFileReadChars < 0 {
		c.Defaults.MaxFileReadChars = d.Defaults.MaxFileReadChars
	}
	if c.Defaults.BashTimeoutSec < 0 {
		c.Defaults.BashTimeoutSec = d.Defaults.BashTimeoutSec
	}
	if c.Defaults.MaxToolRounds < 0 {
		c.Defaults.MaxToolRounds = d.Defaults.MaxToolRounds
	}
	if c.Defaults.MaxTurnRounds < 0 {
		c.Defaults.MaxTurnRounds = d.Defaults.MaxTurnRounds
	}
	// A checkpoint wider than the turn itself could never be reached, so the
	// turn is widened to it. Both sides must be bounded for that to matter.
	if c.Defaults.MaxToolRounds > 0 && c.Defaults.MaxTurnRounds > 0 &&
		c.Defaults.MaxTurnRounds < c.Defaults.MaxToolRounds {
		c.Defaults.MaxTurnRounds = c.Defaults.MaxToolRounds
	}
	if c.Defaults.PlanRounds < 0 {
		c.Defaults.PlanRounds = d.Defaults.PlanRounds
	}
	if c.Defaults.PlanToolCalls < 0 {
		c.Defaults.PlanToolCalls = d.Defaults.PlanToolCalls
	}
	// The BUILD verification gate shares the limit contract: 0 keeps the gate
	// off, a negative value restores the shipped default.
	if c.Defaults.BuildVerifyRounds < 0 {
		c.Defaults.BuildVerifyRounds = d.Defaults.BuildVerifyRounds
	}
	if c.Defaults.AutoContinue == nil {
		c.Defaults.AutoContinue = d.Defaults.AutoContinue
	}
	if c.Defaults.OpenRouterCatalogFallback == nil {
		c.Defaults.OpenRouterCatalogFallback = d.Defaults.OpenRouterCatalogFallback
	}
	if c.Defaults.MaxTurnCostUSD < 0 {
		c.Defaults.MaxTurnCostUSD = 0
	}
	if c.Defaults.MaxConcurrentRuns < 0 {
		c.Defaults.MaxConcurrentRuns = 0
	}
	// Token-budget pruning: 0 disables the budget and re-enables the legacy
	// round-based knobs. Negative values fall back to the shipped default.
	if c.Defaults.HistoryPrunePercent < 0 {
		c.Defaults.HistoryPrunePercent = d.Defaults.HistoryPrunePercent
	}
	if c.Defaults.HistoryPruneMaxTokens < 0 {
		c.Defaults.HistoryPruneMaxTokens = d.Defaults.HistoryPruneMaxTokens
	}
	// 0 keeps every round verbatim, which turns round-based pruning off and
	// leaves the token budget as the only thing that trims the request.
	if c.Defaults.HistoryKeepRecent < 0 {
		c.Defaults.HistoryKeepRecent = 0
	}
	if c.Defaults.HistoryMaxResult < 0 {
		c.Defaults.HistoryMaxResult = d.Defaults.HistoryMaxResult
	}
	if c.Defaults.HistoryMaxArg < 0 {
		c.Defaults.HistoryMaxArg = d.Defaults.HistoryMaxArg
	}
	// 0 is not "no limit" here: it selects the default granularity of the
	// pruning boundary, which is what keeps the provider prompt cache warm.
	if c.Defaults.HistoryPruneChunk < 0 {
		c.Defaults.HistoryPruneChunk = 0
	}
	return c
}

// PruneTokenBudget returns the token ceiling a provider request must fit in:
//
//	min(model context length * history_prune_percent, history_prune_max_tokens)
//
// A zero result means budget-driven pruning is disabled, in which case the
// legacy round-based knobs (history_keep_recent_rounds, history_prune_chunk_rounds)
// decide the window instead. An unknown model context length (0) falls back to
// the token cap alone.
func (d DefaultsConfig) PruneTokenBudget(contextLength int) int {
	limit := 0
	if contextLength > 0 && d.HistoryPrunePercent > 0 {
		limit = contextLength * d.HistoryPrunePercent / 100
	}
	if d.HistoryPruneMaxTokens > 0 && (limit <= 0 || d.HistoryPruneMaxTokens < limit) {
		limit = d.HistoryPruneMaxTokens
	}
	return limit
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
