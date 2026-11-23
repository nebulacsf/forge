// Package llm implements Forge's provider-agnostic LLM layer.
//
// Everything the agent loop needs is expressed by the Provider interface.
// OpenAICompatibleProvider is the single HTTP implementation Forge ships: it
// works with any endpoint that speaks the OpenAI chat-completions protocol,
// including OpenRouter, Ollama, LM Studio, vLLM, Groq, DeepSeek and custom
// gateways.
package llm

import (
	"context"
	"fmt"
	"strings"
)

// DriverOpenAICompatible is the only driver shipped today. Additional drivers
// can be added behind the same Provider interface without touching the agent.
const DriverOpenAICompatible = "openai_compatible"

// Profile is the user-editable description of one configured provider.
type Profile struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Driver       string            `json:"driver"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key,omitempty"`
	DefaultModel string            `json:"default_model"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
}

// StreamDelta is the streaming callback payload. It is an alias so callers can
// use either name while the older Delta type keeps working.
type StreamDelta = Delta

// ChatResponse is the assembled response of one model call.
type ChatResponse = StreamResult

// Provider is the decoupled chat-completions contract used by the agent loop.
// It deliberately exposes no HTTP details.
type Provider interface {
	// Profile returns the configuration this provider was built from.
	Profile() Profile
	// StreamChat sends one request and invokes onDelta for every text fragment.
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(StreamDelta) error) (*ChatResponse, error)
	// FetchModels lists the model catalog advertised by the provider.
	FetchModels(ctx context.Context) ([]ModelInfo, error)
	// Probe checks connectivity and, when supported, lists models. supported is
	// false when the endpoint is reachable but does not expose /models.
	Probe(ctx context.Context) (models []ModelInfo, supported bool, err error)
}

// ChatClient is a readability alias for Provider.
type ChatClient = Provider

// NewProvider builds the concrete client for a profile. Unknown drivers are
// rejected explicitly so a future driver cannot be selected by accident.
func NewProvider(p Profile) (Provider, error) {
	driver := strings.ToLower(strings.TrimSpace(p.Driver))
	if driver == "" {
		driver = DriverOpenAICompatible
	}
	switch driver {
	case DriverOpenAICompatible:
		return New(p), nil
	default:
		return nil, fmt.Errorf("unsupported provider driver %q", p.Driver)
	}
}
