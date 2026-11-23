package llm

// ToolCall is both the wire representation and Forge's persisted form. Result,
// Error, Status and DurationMS are local-only fields and are omitted from
// requests to the provider.
type ToolCall struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"`
	Function   ToolFunction `json:"function"`
	Result     string       `json:"result,omitempty"`
	Error      string       `json:"error,omitempty"`
	Status     string       `json:"status,omitempty"`
	DurationMS int64        `json:"duration_ms,omitempty"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type Tool struct {
	Type     string           `json:"type"`
	Function ToolFunctionSpec `json:"function"`
}

type ToolFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Usage mirrors the OpenAI-compatible usage object. Every field is optional:
// local providers may omit cost, cache details or even the whole object.
type Usage struct {
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	Cost                float64 `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type StreamResult struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
	Usage     Usage
	Model     string
	ID        string
}

// Delta is emitted while text streams in.
type Delta struct {
	Kind string // "reasoning" or "content"
	Text string
}
