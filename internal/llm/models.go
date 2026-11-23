package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrModelsUnsupported means the provider is reachable but does not expose an
// OpenAI-compatible /models endpoint. It is not a connectivity failure.
var ErrModelsUnsupported = errors.New("provider does not expose a /models endpoint")

// ModelInfo is the subset of the provider's model metadata Forge needs.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

// BuiltinModels is the fallback list used when an OpenRouter-shaped provider's
// /models endpoint cannot be reached. It also seeds the model picker before the
// first fetch.
func BuiltinModels() []ModelInfo {
	return []ModelInfo{
		{ID: "qwen/qwen-2.5-coder-32b-instruct", Name: "Qwen2.5 Coder 32B Instruct", ContextLength: 32768},
		{ID: "anthropic/claude-sonnet-4.5", Name: "Claude Sonnet 4.5", ContextLength: 200000},
		{ID: "openai/gpt-5", Name: "GPT-5", ContextLength: 400000},
		{ID: "google/gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextLength: 1048576},
		{ID: "deepseek/deepseek-chat-v3.1", Name: "DeepSeek Chat v3.1", ContextLength: 163840},
		{ID: "x-ai/grok-4", Name: "Grok 4", ContextLength: 256000},
		{ID: "meta-llama/llama-4-maverick", Name: "Llama 4 Maverick", ContextLength: 1048576},
	}
}

// BuiltinContextLength returns the built-in context window for a model ID.
// It reports 0 when the model is unknown, so callers can fall back to a fixed
// token cap instead of guessing.
func BuiltinContextLength(modelID string) int {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return 0
	}
	for _, m := range BuiltinModels() {
		if strings.EqualFold(m.ID, modelID) {
			return m.ContextLength
		}
	}
	return 0
}

// FetchModels lists the models advertised by the configured provider. It
// accepts both the OpenAI `{data:[...]}` envelope and the simpler
// `{models:[...]}` shape some local gateways return.
func (c *OpenAICompatibleProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	if strings.TrimSpace(c.profile.BaseURL) == "" {
		return nil, fmt.Errorf("%s base URL is not configured", c.displayName())
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	endpoint := c.endpoint("/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	c.applyHeaders(req, false)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models from %s: %w", c.displayName(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, ErrModelsUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, providerError(resp.StatusCode, data, c.displayName())
	}

	var payload struct {
		Data   []ModelInfo `json:"data"`
		Models []ModelInfo `json:"models"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models from %s: %w", c.displayName(), err)
	}
	list := payload.Data
	if len(list) == 0 {
		list = payload.Models
	}
	out := make([]ModelInfo, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m.ID = id
		if strings.TrimSpace(m.Name) == "" {
			m.Name = id
		}
		if m.ContextLength < 0 {
			m.ContextLength = 0
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s returned no models", c.displayName())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Probe checks connectivity and model discovery without raising an error when
// the provider simply does not implement /models.
func (c *OpenAICompatibleProvider) Probe(ctx context.Context) ([]ModelInfo, bool, error) {
	models, err := c.FetchModels(ctx)
	if errors.Is(err, ErrModelsUnsupported) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return models, true, nil
}
