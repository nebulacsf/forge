package llm

import "testing"

func TestBuiltinContextLength(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"qwen/qwen-2.5-coder-32b-instruct", 32768},
		{"anthropic/claude-sonnet-4.5", 200000},
		{"google/gemini-2.5-pro", 1048576},
		{"  anthropic/claude-sonnet-4.5  ", 200000},
		{"ANTHROPIC/CLAUDE-SONNET-4.5", 200000},
		{"", 0},
		{"unknown/model", 0},
	}
	for _, c := range cases {
		if got := BuiltinContextLength(c.model); got != c.want {
			t.Errorf("BuiltinContextLength(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
