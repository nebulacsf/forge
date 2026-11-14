package config

import "testing"

func boolValue(v bool) *bool { return &v }

func TestNormalizeRoundBudgetDefaults(t *testing.T) {
	got := normalize(Config{})
	if got.Defaults.MaxToolRounds != 24 {
		t.Errorf("max_tool_rounds = %d, want 24", got.Defaults.MaxToolRounds)
	}
	if got.Defaults.MaxTurnRounds != 96 {
		t.Errorf("max_turn_rounds = %d, want 96", got.Defaults.MaxTurnRounds)
	}
	if got.Defaults.AutoContinue == nil || !*got.Defaults.AutoContinue {
		t.Error("auto_continue should default to true")
	}
	if got.Defaults.MaxTurnCostUSD != 0 {
		t.Errorf("max_turn_cost_usd = %v, want 0 (disabled)", got.Defaults.MaxTurnCostUSD)
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
