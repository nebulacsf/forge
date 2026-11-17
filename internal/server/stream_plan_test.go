package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forge/internal/config"
	"forge/internal/llm"
	"forge/internal/store"
)

func (m *planFinalizeMock) respond(w http.ResponseWriter, flusher http.Flusher, send func(any), n int, toolChoice string, noTools bool) {
	if toolChoice == "none" || noTools {
		attempt := int(m.finalSeen.Add(1))
		if m.invalidFinalUnlessNoTools && !noTools {
			m.writePseudoFinal(w, flusher, send)
			return
		}
		if m.invalidFinalOnce && attempt == 1 {
			m.writePseudoFinal(w, flusher, send)
			return
		}
		if m.emptyFinalOnce && attempt == 1 {
			m.writeEmptyFinal(w, flusher, send)
			return
		}
		m.writeFinal(w, flusher, send)
		return
	}
	if m.pseudoResearchOnce && n == 1 {
		m.writePseudoResearch(w, flusher, send)
		return
	}
	m.writeResearch(w, flusher, send, n)
}

func (m *planFinalizeMock) writeEmptyFinal(w http.ResponseWriter, flusher http.Flusher, send func(any)) {
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{"reasoning": "no text"}},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13, "cost": 0.0001,
	}})
}

func (m *planFinalizeMock) writePseudoFinal(w http.ResponseWriter, flusher http.Flusher, send func(any)) {
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{
			"reasoning": "I still need one more grep.",
			"content":   `<tool_call><function=grep_search><parameter=pattern>renderMarkdownInto</parameter></function></tool_call>`,
		}},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 6, "total_tokens": 16, "cost": 0.0001,
	}})
}

func (m *planFinalizeMock) writePseudoResearch(w http.ResponseWriter, flusher http.Flusher, send func(any)) {
	send(map[string]any{"id": "gen-1", "choices": []any{
		map[string]any{"delta": map[string]any{
			"reasoning": "I should search for the renderer.",
			"content":   `<tool_call><function=grep_search><parameter=pattern>renderMarkdownInto</parameter></function></tool_call>`,
		}},
	}})
	send(map[string]any{"id": "gen-1", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-1", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 6, "total_tokens": 16, "cost": 0.0001,
	}})
}

func (m *planFinalizeMock) writeFinal(w http.ResponseWriter, flusher http.Flusher, send func(any)) {
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "## Diagnosis & Approach\nThe message renderer lacks an action row. Add one shared helper.\n\n## Key Decisions\n- Reuse the existing clipboard helper.\n\n## Implementation Plan\n- **web/js/app.js (lines 640-700)**: Add the action row to each message.\n\nReady to implement — switch to BUILD to apply this plan."}},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{
			"tool_calls": []any{
				map[string]any{"index": 0, "id": "ignored-final", "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": `{"path":"main.go","start_line":1,"end_line":5}`,
				}},
			},
		}},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18, "cost": 0.0001,
	}})
}

func (m *planFinalizeMock) writeResearch(w http.ResponseWriter, flusher http.Flusher, send func(any), n int) {
	start := 1 + (n-1)*5
	end := start + 4
	if m.sameRange {
		start, end = 1, 5
	}
	args := fmt.Sprintf(`{"path":"main.go","start_line":%d,"end_line":%d}`, start, end)
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{
		map[string]any{"delta": map[string]any{
			"tool_calls": []any{
				map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": args,
				}},
			},
		}},
	}})
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
	}})
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0.0001,
	}})
}

func TestPlanKeepRecentAboveLegacyCap(t *testing.T) {
	opts := normalizeHistoryOptions(historyOptions{KeepRecent: 26})
	if opts.KeepRecent != 26 {
		t.Errorf("keep recent = %d, want 26 (plan_rounds=24 plus two)", opts.KeepRecent)
	}
}

func TestEffectiveHistoryResultLimit(t *testing.T) {
	cases := []struct {
		mode       string
		configured int
		want       int
	}{
		{"PLAN", 4000, planMinResultChars},
		{"PLAN", 0, 0},
		{"PLAN", 12000, 12000},
		{"BUILD", 4000, 4000},
	}
	for _, c := range cases {
		if got := effectiveHistoryResultLimit(c.mode, c.configured); got != c.want {
			t.Errorf("effectiveHistoryResultLimit(%q, %d) = %d, want %d",
				c.mode, c.configured, got, c.want)
		}
	}
}

func TestPlanHistoryResultLimitKeepsMiddle(t *testing.T) {
	result := strings.Repeat("a", 2500) + "MIDDLE" + strings.Repeat("b", 2500)
	calls := []llm.ToolCall{{
		ID:     "c1",
		Type:   "function",
		Status: "done",
		Function: llm.ToolFunction{
			Name:      "read_file",
			Arguments: `{"path":"main.go","start_line":1,"end_line":100}`,
		},
		Result: result,
	}}
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.Message{
		{ID: 1, Role: "user", Content: "inspect"},
		{ID: 2, Role: "assistant", ToolCalls: string(raw)},
	}
	history, _ := buildHistory(rows, "system", historyOptions{
		KeepRecent:     10,
		MaxResultChars: effectiveHistoryResultLimit("PLAN", 4000),
	})
	for _, msg := range history {
		if msg.Role != "tool" {
			continue
		}
		if !strings.Contains(msg.Content, "MIDDLE") {
			t.Fatalf("PLAN result was clipped in the middle: %d chars", len(msg.Content))
		}
		return
	}
	t.Fatal("tool message not found in history")
}

func TestLooksLikePseudoToolCall(t *testing.T) {
	for _, s := range []string{
		`<tool_call><function=grep_search></function></tool_call>`,
		`<function=read_file>`,
		`{"name":"read_file","arguments":"{}"}`,
	} {
		if !looksLikePseudoToolCall(s) {
			t.Errorf("looksLikePseudoToolCall(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"## Diagnosis & Approach: add copy buttons to the UI and verify the behavior.",
		`{"plan":"add buttons"}`,
	} {
		if looksLikePseudoToolCall(s) {
			t.Errorf("looksLikePseudoToolCall(%q) = true, want false", s)
		}
	}
	if usablePlanContent("Done.") {
		t.Error("short content should not be a usable plan")
	}
	if !usablePlanContent("## Diagnosis & Approach: add copy buttons to code blocks and messages, then verify the result.") {
		t.Error("a normal plan should be usable")
	}
}

func TestChatStreamPlanForcesFinalAnswerAtBudget(t *testing.T) {
	mock := &planFinalizeMock{}
	app, st, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 6
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 7 {
		t.Errorf("round_start = %d, want 7 (6 research + final)", got)
	}
	if got := countEvents(events, "tool_end"); got != 6 {
		t.Errorf("tool_end = %d, want 6 (final tool call must be ignored)", got)
	}
	if got := countEvents(events, "limit"); got != 0 {
		t.Errorf("limit = %d, want 0", got)
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	if !mock.sawToolChoiceNone.Load() {
		t.Error("final request did not use tool_choice=none")
	}
	if !mock.sawFirstResult.Load() {
		t.Error("final request lost the first research result (history was pruned)")
	}
	if mock.sawPruned.Load() {
		t.Error("final request contained pruned tool output")
	}
	if v := mock.finalNote.Load(); v == nil || !strings.Contains(v.(string), "research budget exhausted") {
		t.Errorf("final note = %v, want the budget reason", v)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 8 {
		t.Fatalf("messages = %d, want 8 (user + 6 tool rounds + final)", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "## Diagnosis & Approach") {
		t.Errorf("last message = %+v, want the final plan", last)
	}
}

func TestChatStreamPlanToolCallBudgetForcesFinal(t *testing.T) {
	mock := &planFinalizeMock{}
	app, _, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 12
		c.Defaults.PlanToolCalls = 2
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 3 {
		t.Errorf("round_start = %d, want 3 (2 research + final)", got)
	}
	if got := countEvents(events, "tool_end"); got != 2 {
		t.Errorf("tool_end = %d, want 2", got)
	}
	if !mock.sawToolChoiceNone.Load() {
		t.Error("final request did not use tool_choice=none")
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
}

func TestChatStreamPlanGuardForcesFinalInsteadOfAbort(t *testing.T) {
	mock := &planFinalizeMock{sameRange: true}
	app, _, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 8
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "inspect"})

	if got := countEvents(events, "round_start"); got != loopAbortRepeats+1 {
		t.Errorf("round_start = %d, want %d (loop stop + final)", got, loopAbortRepeats+1)
	}
	if got := countEvents(events, "tool_end"); got != loopAbortRepeats {
		t.Errorf("tool_end = %d, want %d", got, loopAbortRepeats)
	}
	if got := countEvents(events, "limit"); got != 0 {
		t.Errorf("limit = %d, want 0 (PLAN finalizes instead of aborting)", got)
	}
	if !mock.sawToolChoiceNone.Load() {
		t.Error("guard-triggered final request did not use tool_choice=none")
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content after the guard")
	}
}

func TestChatStreamPlanFinalRetriesEmptyAnswer(t *testing.T) {
	mock := &planFinalizeMock{emptyFinalOnce: true}
	app, st, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 2
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 4 {
		t.Errorf("round_start = %d, want 4 (2 research + 2 final attempts)", got)
	}
	if got := countEvents(events, "tool_end"); got != 2 {
		t.Errorf("tool_end = %d, want 2", got)
	}
	if mock.finalSeen.Load() != 2 {
		t.Errorf("final attempts = %d, want 2", mock.finalSeen.Load())
	}
	if !mock.sawToolChoiceNone.Load() {
		t.Error("final request did not use tool_choice=none")
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content after the retry")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "## Diagnosis & Approach") {
		t.Errorf("last message = %+v, want the final plan", last)
	}
}

func TestChatStreamPlanRejectsPseudoFinalContent(t *testing.T) {
	mock := &planFinalizeMock{invalidFinalOnce: true}
	app, st, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 2
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 4 {
		t.Errorf("round_start = %d, want 4 (2 research + 2 final attempts)", got)
	}
	if mock.finalSeen.Load() != 2 {
		t.Errorf("final attempts = %d, want 2", mock.finalSeen.Load())
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content after rejecting the pseudo tool call")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	if v := mock.finalNote.Load(); v == nil || !strings.Contains(v.(string), "not a usable plan") {
		t.Errorf("retry note = %v, want the unusable-plan warning", v)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if strings.Contains(last.Content, "<tool_call>") {
		t.Errorf("pseudo tool call was persisted as the final answer: %q", last.Content)
	}
	if !strings.Contains(last.Content, "## Diagnosis & Approach") {
		t.Errorf("last message = %+v, want the final plan", last)
	}
}

func TestChatStreamPlanLastFinalAttemptHasNoTools(t *testing.T) {
	mock := &planFinalizeMock{invalidFinalUnlessNoTools: true}
	app, _, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 2
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "round_start"); got != 5 {
		t.Errorf("round_start = %d, want 5 (2 research + 3 final attempts)", got)
	}
	if mock.finalSeen.Load() != 3 {
		t.Errorf("final attempts = %d, want 3", mock.finalSeen.Load())
	}
	if !mock.sawNoToolsFinal.Load() {
		t.Error("last final attempt did not omit tool definitions")
	}
	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected final plan content on the no-tools attempt")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
}

func TestChatStreamPlanRejectsPseudoToolCallDuringResearch(t *testing.T) {
	mock := &planFinalizeMock{pseudoResearchOnce: true}
	app, st, session := chatBudgetFixture(t, "PLAN", mock, func(c *config.Config) {
		c.Defaults.PlanRounds = 3
		c.Defaults.PlanToolCalls = 100
		c.Defaults.MaxTurnRounds = 96
		c.Defaults.AutoContinue = boolValue(true)
		c.Defaults.HistoryPruneChunk = 1
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "plan it"})

	if got := countEvents(events, "content"); got == 0 {
		t.Error("expected a final plan")
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	if got := countEvents(events, "error"); got != 0 {
		t.Errorf("error events = %d, want 0", got)
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "<tool_call>") {
			t.Errorf("pseudo tool call leaked into persisted content: %q", msg.Content)
		}
	}
}
