package server

import (
	"fmt"
	"strings"
	"testing"

	"forge/internal/config"
	"forge/internal/store"
)

// Without a research budget PLAN has nothing to finalize for: the turn keeps
// calling tools and only stops at the configured turn ceiling, with the model
// never forced into a final answer.
func TestChatStreamPlanWithoutResearchBudget(t *testing.T) {
	mock := &planFinalizeMock{}
	app, _, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 0
		c.Defaults.PlanToolCalls = 0
		c.Defaults.MaxToolRounds = 0
		c.Defaults.MaxTurnRounds = 3
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 3 {
		t.Errorf("round_start = %d, want 3 (only the turn ceiling stops it)", got)
	}
	if got := countEvents(events, "tool_end"); got != 3 {
		t.Errorf("tool_end = %d, want 3 (tools stay available for every round)", got)
	}
	if mock.sawToolChoiceNone.Load() {
		t.Error("no research budget was configured, so no final answer should be forced")
	}
	if v := mock.finalNote.Load(); v != nil {
		t.Errorf("final note = %v, want none without a research budget", v)
	}
	limit := findEvent(events, "limit")
	if limit == nil || limit["reason"] != "turn_budget" {
		t.Fatalf("limit = %+v, want the turn budget reason", limit)
	}
	if got := countEvents(events, "checkpoint"); got != 0 {
		t.Errorf("checkpoint = %d, want 0 (PLAN never auto-continues)", got)
	}
}

// A zero research budget on one axis must not silently disable the other one.
func TestChatStreamPlanKeepsRemainingBudgetAxis(t *testing.T) {
	mock := &planFinalizeMock{}
	app, _, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 0
		c.Defaults.PlanToolCalls = 1
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "tool_end"); got != 1 {
		t.Errorf("tool_end = %d, want 1 (the tool-call cap is still enforced)", got)
	}
	if !mock.sawToolChoiceNone.Load() {
		t.Error("the tool-call cap should still force the final plan")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
}

func TestBuildHistoryKeepRecentZeroKeepsEverything(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 6; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
			strings.Repeat("x", 500)))
	}
	// Token budget disabled and no round window: nothing may be stubbed.
	got, stats := buildHistory(rows, "system", historyOptions{KeepRecent: 0})
	if stats.PrunedRounds != 0 {
		t.Errorf("PrunedRounds = %d, want 0 (every round kept verbatim)", stats.PrunedRounds)
	}
	for _, m := range got {
		if strings.HasPrefix(m.Content, stubPrefix) {
			t.Errorf("round stubbed with KeepRecent 0: %q", m.Content)
		}
	}
	assertNoOrphanToolMessages(t, got)
}

func TestNormalizeHistoryOptionsKeepsZeroKeepRecent(t *testing.T) {
	opts := normalizeHistoryOptions(historyOptions{KeepRecent: 0})
	if opts.KeepRecent != 0 {
		t.Errorf("KeepRecent = %d, want 0 (no round window)", opts.KeepRecent)
	}
	opts = normalizeHistoryOptions(historyOptions{KeepRecent: -1})
	if opts.KeepRecent != defaultHistoryKeepRecent {
		t.Errorf("KeepRecent = %d, want %d for a negative value", opts.KeepRecent, defaultHistoryKeepRecent)
	}
}
