package config

import "testing"

func boolValue(v bool) *bool { return &v }

func TestDefaultShipsFiniteLimits(t *testing.T) {
	// The shipped configuration stays bounded: an unbounded turn is something
	// the developer opts into by writing 0, never the out-of-the-box behavior.
	d := Default().Defaults
	if d.MaxToolRounds != 16 || d.MaxTurnRounds != 64 {
		t.Errorf("round budgets = %d/%d, want 16/64", d.MaxToolRounds, d.MaxTurnRounds)
	}
	if d.PlanRounds != 16 || d.PlanToolCalls != 32 {
		t.Errorf("plan budget = %d/%d, want 16/32", d.PlanRounds, d.PlanToolCalls)
	}
	if d.MaxFileReadChars != 25000 || d.BashTimeoutSec != 30 {
		t.Errorf("tool limits = %d/%d, want 25000/30", d.MaxFileReadChars, d.BashTimeoutSec)
	}
	if d.AutoContinue == nil || !*d.AutoContinue {
		t.Error("auto_continue should default to true")
	}
	if d.MaxTurnCostUSD != 0 {
		t.Errorf("max_turn_cost_usd = %v, want 0 (no limit)", d.MaxTurnCostUSD)
	}
	if d.BuildVerifyRounds != 2 {
		t.Errorf("build_verify_rounds = %d, want 2", d.BuildVerifyRounds)
	}
}

func TestNormalizeKeepsZeroLimitsAsUnlimited(t *testing.T) {
	// A config file where every limit was explicitly cleared must survive with
	// its zeros intact: normalize is not allowed to reintroduce a ceiling.
	var c Config
	c.Defaults.Model = DefaultModel
	got := normalize(c)
	for _, knob := range limitKnobs() {
		if v := knob.get(got.Defaults); v != 0 {
			t.Errorf("%s = %d, want 0 (no limit) preserved from an explicit zero", knob.name, v)
		}
	}
	if got.Defaults.AutoContinue == nil || !*got.Defaults.AutoContinue {
		t.Error("auto_continue should still fall back to true")
	}
}

// limitKnob describes one numeric default so the contract can be checked
// uniformly across every limit: an explicit 0 survives as "no limit", a huge
// value is never clamped, and a negative value means "not configured".
type limitKnob struct {
	name  string
	set   func(*DefaultsConfig, int)
	get   func(DefaultsConfig) int
	fallb int
}

func limitKnobs() []limitKnob {
	d := Default().Defaults
	return []limitKnob{
		{"max_file_read_chars", func(c *DefaultsConfig, v int) { c.MaxFileReadChars = v },
			func(c DefaultsConfig) int { return c.MaxFileReadChars }, d.MaxFileReadChars},
		{"bash_timeout_sec", func(c *DefaultsConfig, v int) { c.BashTimeoutSec = v },
			func(c DefaultsConfig) int { return c.BashTimeoutSec }, d.BashTimeoutSec},
		{"max_tool_rounds", func(c *DefaultsConfig, v int) { c.MaxToolRounds = v },
			func(c DefaultsConfig) int { return c.MaxToolRounds }, d.MaxToolRounds},
		{"max_turn_rounds", func(c *DefaultsConfig, v int) { c.MaxTurnRounds = v },
			func(c DefaultsConfig) int { return c.MaxTurnRounds }, d.MaxTurnRounds},
		{"plan_rounds", func(c *DefaultsConfig, v int) { c.PlanRounds = v },
			func(c DefaultsConfig) int { return c.PlanRounds }, d.PlanRounds},
		{"plan_tool_calls", func(c *DefaultsConfig, v int) { c.PlanToolCalls = v },
			func(c DefaultsConfig) int { return c.PlanToolCalls }, d.PlanToolCalls},
		{"build_verify_rounds", func(c *DefaultsConfig, v int) { c.BuildVerifyRounds = v },
			func(c DefaultsConfig) int { return c.BuildVerifyRounds }, d.BuildVerifyRounds},
		{"history_prune_max_tokens", func(c *DefaultsConfig, v int) { c.HistoryPruneMaxTokens = v },
			func(c DefaultsConfig) int { return c.HistoryPruneMaxTokens }, d.HistoryPruneMaxTokens},
		{"history_max_result_chars", func(c *DefaultsConfig, v int) { c.HistoryMaxResult = v },
			func(c DefaultsConfig) int { return c.HistoryMaxResult }, d.HistoryMaxResult},
		{"history_max_arg_chars", func(c *DefaultsConfig, v int) { c.HistoryMaxArg = v },
			func(c DefaultsConfig) int { return c.HistoryMaxArg }, d.HistoryMaxArg},
	}
}

func TestNormalizeLimitContract(t *testing.T) {
	// Every knob is tested with a value far above the ceilings this code used
	// to enforce, so a reintroduced clamp fails here.
	const huge = 9_000_000
	for _, knob := range limitKnobs() {
		t.Run(knob.name, func(t *testing.T) {
			c := Default()
			knob.set(&c.Defaults, 0)
			if got := knob.get(normalize(c).Defaults); got != 0 {
				t.Errorf("0 should mean no limit, got %d", got)
			}
			c = Default()
			knob.set(&c.Defaults, huge)
			if got := knob.get(normalize(c).Defaults); got != huge {
				t.Errorf("%d should be preserved verbatim, got %d", huge, got)
			}
			c = Default()
			knob.set(&c.Defaults, -5)
			if got := knob.get(normalize(c).Defaults); got != knob.fallb {
				t.Errorf("negative should fall back to %d, got %d", knob.fallb, got)
			}
		})
	}
}

func TestNormalizePlanBudgetContract(t *testing.T) {
	c := Default()
	c.Defaults.PlanRounds = 0
	c.Defaults.PlanToolCalls = 0
	got := normalize(c)
	if got.Defaults.PlanRounds != 0 || got.Defaults.PlanToolCalls != 0 {
		t.Errorf("zero plan budget should stay unlimited, got rounds=%d calls=%d",
			got.Defaults.PlanRounds, got.Defaults.PlanToolCalls)
	}

	c = Default()
	c.Defaults.PlanRounds = -2
	c.Defaults.PlanToolCalls = -2
	got = normalize(c)
	if got.Defaults.PlanRounds != 16 || got.Defaults.PlanToolCalls != 32 {
		t.Errorf("negative plan budget should fall back to defaults, got rounds=%d calls=%d",
			got.Defaults.PlanRounds, got.Defaults.PlanToolCalls)
	}
}

func TestNormalizeRoundBudgetsWithoutCeilings(t *testing.T) {
	c := Default()
	c.Defaults.MaxToolRounds = 500
	c.Defaults.MaxTurnRounds = 500
	got := normalize(c)
	if got.Defaults.MaxToolRounds != 500 || got.Defaults.MaxTurnRounds != 500 {
		t.Errorf("round budgets should not be clamped, got segment=%d turn=%d",
			got.Defaults.MaxToolRounds, got.Defaults.MaxTurnRounds)
	}
}

func TestNormalizeOpenRouterCatalogFallback(t *testing.T) {
	// The fallback ships on, so a config file written before the knob existed
	// (nil) must resolve to enabled instead of silently turning it off.
	if !Default().Defaults.OpenRouterCatalogEnabled() {
		t.Error("openrouter_catalog_fallback should ship enabled")
	}
	c := Default()
	c.Defaults.OpenRouterCatalogFallback = nil
	if !normalize(c).Defaults.OpenRouterCatalogEnabled() {
		t.Error("a nil openrouter_catalog_fallback should normalize to enabled")
	}
	c.Defaults.OpenRouterCatalogFallback = boolValue(false)
	if normalize(c).Defaults.OpenRouterCatalogEnabled() {
		t.Error("an explicit openrouter_catalog_fallback=false was lost")
	}
}

func TestNormalizeKeepsExplicitAutoContinueFalse(t *testing.T) {
	c := Default()
	c.Defaults.AutoContinue = boolValue(false)
	got := normalize(c)
	if got.Defaults.AutoContinue == nil || *got.Defaults.AutoContinue {
		t.Error("explicit auto_continue=false was lost during normalization")
	}
}

func TestNormalizeRaisesTurnBudgetToSegment(t *testing.T) {
	c := Default()
	c.Defaults.MaxToolRounds = 40
	c.Defaults.MaxTurnRounds = 10
	got := normalize(c)
	if got.Defaults.MaxTurnRounds != 40 {
		t.Errorf("max_turn_rounds = %d, want 40", got.Defaults.MaxTurnRounds)
	}
}

func TestNormalizeRejectsNegativeCostCap(t *testing.T) {
	c := Default()
	c.Defaults.MaxTurnCostUSD = -3
	got := normalize(c)
	if got.Defaults.MaxTurnCostUSD != 0 {
		t.Errorf("max_turn_cost_usd = %v, want 0", got.Defaults.MaxTurnCostUSD)
	}
}

func TestNormalizeHistoryPruningDefaults(t *testing.T) {
	got := normalize(Default())
	if got.Defaults.HistoryPrunePercent != 70 {
		t.Errorf("history_prune_percent = %d, want 70", got.Defaults.HistoryPrunePercent)
	}
	if got.Defaults.HistoryPruneMaxTokens != 120000 {
		t.Errorf("history_prune_max_tokens = %d, want 120000", got.Defaults.HistoryPruneMaxTokens)
	}
	// Round-based pruning ships disabled: the token budget drives the window.
	if got.Defaults.HistoryKeepRecent != 0 {
		t.Errorf("history_keep_recent_rounds = %d, want 0 (disabled)", got.Defaults.HistoryKeepRecent)
	}
	if got.Defaults.HistoryPruneChunk != 0 {
		t.Errorf("history_prune_chunk_rounds = %d, want 0 (disabled)", got.Defaults.HistoryPruneChunk)
	}
	if got.Defaults.HistoryMaxResult != 16000 {
		t.Errorf("history_max_result_chars = %d, want 16000", got.Defaults.HistoryMaxResult)
	}
	if got.Defaults.HistoryMaxArg != 4000 {
		t.Errorf("history_max_arg_chars = %d, want 4000", got.Defaults.HistoryMaxArg)
	}
}

func TestNormalizeHistoryPruningWithoutCeilings(t *testing.T) {
	c := Default()
	c.Defaults.HistoryKeepRecent = 150
	c.Defaults.HistoryMaxResult = 10
	c.Defaults.HistoryMaxArg = 40
	c.Defaults.HistoryPruneChunk = 150
	got := normalize(c)
	if got.Defaults.HistoryKeepRecent != 150 {
		t.Errorf("history_keep_recent_rounds = %d, want 150 (no ceiling)", got.Defaults.HistoryKeepRecent)
	}
	if got.Defaults.HistoryMaxResult != 10 {
		t.Errorf("history_max_result_chars = %d, want 10 (no floor)", got.Defaults.HistoryMaxResult)
	}
	if got.Defaults.HistoryMaxArg != 40 {
		t.Errorf("history_max_arg_chars = %d, want 40 (no floor)", got.Defaults.HistoryMaxArg)
	}
	if got.Defaults.HistoryPruneChunk != 150 {
		t.Errorf("history_prune_chunk_rounds = %d, want 150 (no ceiling)", got.Defaults.HistoryPruneChunk)
	}
}

func TestNormalizeHistoryNegativesFallBack(t *testing.T) {
	c := Default()
	c.Defaults.HistoryKeepRecent = -1
	c.Defaults.HistoryMaxArg = -5
	got := normalize(c)
	if got.Defaults.HistoryKeepRecent != 0 {
		t.Errorf("history_keep_recent_rounds = %d, want 0 (keep everything)", got.Defaults.HistoryKeepRecent)
	}
	if got.Defaults.HistoryMaxArg != 4000 {
		t.Errorf("history_max_arg_chars = %d, want default 4000 for negative input", got.Defaults.HistoryMaxArg)
	}
}

func TestNormalizeHistoryLegacyChunkStaysDisabled(t *testing.T) {
	c := Default()
	c.Defaults.HistoryPruneChunk = 0
	if got := normalize(c); got.Defaults.HistoryPruneChunk != 0 {
		t.Errorf("history_prune_chunk_rounds = %d, want 0 (default granularity) for zero input", got.Defaults.HistoryPruneChunk)
	}
	c.Defaults.HistoryPruneChunk = -3
	if got := normalize(c); got.Defaults.HistoryPruneChunk != 0 {
		t.Errorf("history_prune_chunk_rounds = %d, want 0 (default granularity) for negative input", got.Defaults.HistoryPruneChunk)
	}
}

func TestNormalizePruneBudgetWithoutCeilings(t *testing.T) {
	c := Default()
	c.Defaults.HistoryPrunePercent = 500
	c.Defaults.HistoryPruneMaxTokens = 9_000_000
	got := normalize(c)
	if got.Defaults.HistoryPrunePercent != 500 {
		t.Errorf("history_prune_percent = %d, want 500 (no ceiling)", got.Defaults.HistoryPrunePercent)
	}
	if got.Defaults.HistoryPruneMaxTokens != 9_000_000 {
		t.Errorf("history_prune_max_tokens = %d, want 9000000 (no ceiling)", got.Defaults.HistoryPruneMaxTokens)
	}

	c = Default()
	c.Defaults.HistoryPrunePercent = -10
	c.Defaults.HistoryPruneMaxTokens = -1
	got = normalize(c)
	if got.Defaults.HistoryPrunePercent != 70 || got.Defaults.HistoryPruneMaxTokens != 120000 {
		t.Errorf("negative values should fall back to defaults, got percent=%d tokens=%d",
			got.Defaults.HistoryPrunePercent, got.Defaults.HistoryPruneMaxTokens)
	}
}

func TestPruneTokenBudget(t *testing.T) {
	cases := []struct {
		name     string
		percent  int
		maxToken int
		context  int
		want     int
	}{
		{"small model wins", 70, 120000, 32768, 22937},
		{"cap wins on huge model", 70, 120000, 1_048_576, 120000},
		{"unknown model uses cap", 70, 120000, 0, 120000},
		{"percent disabled uses cap", 0, 120000, 200000, 120000},
		{"cap disabled uses percent", 70, 0, 200000, 140000},
		{"both disabled", 0, 0, 200000, 0},
		{"unknown model and no cap", 70, 0, 0, 0},
	}
	for _, c := range cases {
		d := DefaultsConfig{HistoryPrunePercent: c.percent, HistoryPruneMaxTokens: c.maxToken}
		if got := d.PruneTokenBudget(c.context); got != c.want {
			t.Errorf("%s: PruneTokenBudget(%d) = %d, want %d", c.name, c.context, got, c.want)
		}
	}
}

func TestNormalizeHistoryZeroDisablesCap(t *testing.T) {
	c := Default()
	c.Defaults.HistoryMaxResult = 0
	c.Defaults.HistoryMaxArg = 0
	got := normalize(c)
	if got.Defaults.HistoryMaxResult != 0 || got.Defaults.HistoryMaxArg != 0 {
		t.Errorf("zero should disable caps, got result=%d args=%d",
			got.Defaults.HistoryMaxResult, got.Defaults.HistoryMaxArg)
	}
}
