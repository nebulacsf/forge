package config

import "testing"

func boolValue(v bool) *bool { return &v }

func TestNormalizeRoundBudgetDefaults(t *testing.T) {
	got := normalize(Config{})
	if got.Defaults.MaxToolRounds != 16 {
		t.Errorf("max_tool_rounds = %d, want 16", got.Defaults.MaxToolRounds)
	}
	if got.Defaults.MaxTurnRounds != 64 {
		t.Errorf("max_turn_rounds = %d, want 64", got.Defaults.MaxTurnRounds)
	}
	if got.Defaults.AutoContinue == nil || !*got.Defaults.AutoContinue {
		t.Error("auto_continue should default to true")
	}
	if got.Defaults.MaxTurnCostUSD != 0 {
		t.Errorf("max_turn_cost_usd = %v, want 0 (disabled)", got.Defaults.MaxTurnCostUSD)
	}
	if got.Defaults.PlanRounds != 16 {
		t.Errorf("plan_rounds = %d, want 16", got.Defaults.PlanRounds)
	}
	if got.Defaults.PlanToolCalls != 32 {
		t.Errorf("plan_tool_calls = %d, want 32", got.Defaults.PlanToolCalls)
	}
}

func TestNormalizeClampsPlanBudget(t *testing.T) {
	c := Default()
	c.Defaults.PlanRounds = 99
	c.Defaults.PlanToolCalls = 999
	got := normalize(c)
	if got.Defaults.PlanRounds != 24 {
		t.Errorf("plan_rounds = %d, want 24", got.Defaults.PlanRounds)
	}
	if got.Defaults.PlanToolCalls != 200 {
		t.Errorf("plan_tool_calls = %d, want 200", got.Defaults.PlanToolCalls)
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

func TestNormalizeClampsRoundBudgets(t *testing.T) {
	c := Default()
	c.Defaults.MaxToolRounds = 500
	c.Defaults.MaxTurnRounds = 10
	got := normalize(c)
	if got.Defaults.MaxToolRounds != 200 {
		t.Errorf("max_tool_rounds = %d, want 200", got.Defaults.MaxToolRounds)
	}
	if got.Defaults.MaxTurnRounds != 200 {
		t.Errorf("max_turn_rounds = %d, want 200 (raised to the segment size)", got.Defaults.MaxTurnRounds)
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

func TestNormalizeClampsHistoryPruning(t *testing.T) {
	c := Default()
	c.Defaults.HistoryKeepRecent = 150
	c.Defaults.HistoryMaxResult = 10
	c.Defaults.HistoryMaxArg = -5
	c.Defaults.HistoryPruneChunk = 150
	got := normalize(c)
	if got.Defaults.HistoryKeepRecent != 100 {
		t.Errorf("history_keep_recent_rounds = %d, want 100", got.Defaults.HistoryKeepRecent)
	}
	if got.Defaults.HistoryMaxResult != 500 {
		t.Errorf("history_max_result_chars = %d, want 500", got.Defaults.HistoryMaxResult)
	}
	if got.Defaults.HistoryMaxArg != 4000 {
		t.Errorf("history_max_arg_chars = %d, want default 4000 for negative input", got.Defaults.HistoryMaxArg)
	}
	if got.Defaults.HistoryPruneChunk != 100 {
		t.Errorf("history_prune_chunk_rounds = %d, want 100", got.Defaults.HistoryPruneChunk)
	}
}

func TestNormalizeHistoryLegacyChunkStaysDisabled(t *testing.T) {
	c := Default()
	c.Defaults.HistoryPruneChunk = 0
	if got := normalize(c); got.Defaults.HistoryPruneChunk != 0 {
		t.Errorf("history_prune_chunk_rounds = %d, want 0 (disabled) for zero input", got.Defaults.HistoryPruneChunk)
	}
	c.Defaults.HistoryPruneChunk = -3
	if got := normalize(c); got.Defaults.HistoryPruneChunk != 0 {
		t.Errorf("history_prune_chunk_rounds = %d, want 0 (disabled) for negative input", got.Defaults.HistoryPruneChunk)
	}
}

func TestNormalizePruneBudgetBounds(t *testing.T) {
	c := Default()
	c.Defaults.HistoryPrunePercent = 500
	c.Defaults.HistoryPruneMaxTokens = 9_000_000
	got := normalize(c)
	if got.Defaults.HistoryPrunePercent != 100 {
		t.Errorf("history_prune_percent = %d, want 100", got.Defaults.HistoryPrunePercent)
	}
	if got.Defaults.HistoryPruneMaxTokens != 2_000_000 {
		t.Errorf("history_prune_max_tokens = %d, want 2000000", got.Defaults.HistoryPruneMaxTokens)
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
