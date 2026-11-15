package server

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// looksLikePseudoToolCall detects tool calls written into the response text.
// This happens with some providers when native tool calling is disabled: the
// model emits <tool_call>... or a JSON tool-call object as content instead of
// answering. Such content must never be accepted as a final plan.
func looksLikePseudoToolCall(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	lower := strings.ToLower(t)
	for _, prefix := range []string{
		"<tool_call", "<function", "</tool_call", "```tool", "<|tool_call",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	if strings.HasPrefix(lower, "{") {
		var v struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal([]byte(t), &v) == nil &&
			(v.Name != "" || len(v.Arguments) > 0 || len(v.ToolCalls) > 0) {
			return true
		}
	}
	if strings.HasPrefix(lower, "[") &&
		(strings.Contains(lower, `"function"`) || strings.Contains(lower, `"tool_calls"`)) {
		return true
	}
	return false
}

// usablePlanContent rejects empty answers, pseudo tool calls, and fragments too
// short to be a plan. It stays lenient about the exact plan format so a concise
// but valid Markdown plan is still accepted.
func usablePlanContent(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || looksLikePseudoToolCall(t) {
		return false
	}
	return utf8.RuneCountInString(t) >= 40
}
