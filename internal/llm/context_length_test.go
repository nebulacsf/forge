package llm

import "testing"

func TestContextLengthFor(t *testing.T) {
	catalog := []ModelInfo{
		{ID: "anthropic/claude-sonnet-4.5", ContextLength: 200000},
		{ID: "deepseek/deepseek-chat-v3.1", ContextLength: 163840},
		{ID: "gateway-only/model-without-window"},
	}
	cases := []struct {
		name  string
		model string
		want  int
	}{
		{"exact id", "anthropic/claude-sonnet-4.5", 200000},
		{"exact id ignores case", "ANTHROPIC/CLAUDE-SONNET-4.5", 200000},
		{"padded id", "  deepseek/deepseek-chat-v3.1  ", 163840},
		// A gateway that lists bare slugs must still match a prefixed id, because
		// that is how a subscription endpoint names the same upstream model.
		{"bare slug matches prefixed id", "claude-sonnet-4.5", 200000},
		{"entry without a window", "gateway-only/model-without-window", 0},
		{"unknown model", "nobody/nothing", 0},
		{"empty model", "   ", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContextLengthFor(catalog, c.model); got != c.want {
				t.Errorf("ContextLengthFor(%q) = %d, want %d", c.model, got, c.want)
			}
		})
	}
	if got := ContextLengthFor(nil, "anthropic/claude-sonnet-4.5"); got != 0 {
		t.Errorf("empty catalog = %d, want 0", got)
	}
}
