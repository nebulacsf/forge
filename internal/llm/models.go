package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo is the subset of OpenRouter's model metadata Forge needs.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

// BuiltinModels is the fallback list used when the provider's /models endpoint
// cannot be reached. It also seeds the model picker before the first fetch.
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

// FetchModels lists the models advertised by the configured provider.
func (c *Client) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("HTTP-Referer", "https://github.com/forge-harness/forge")
	req.Header.Set("X-Title", "Forge")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, providerError(resp.StatusCode, data)
	}

	var payload struct {
		Data []ModelInfo `json:"data"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	out := make([]ModelInfo, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, m := range payload.Data {
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
		return nil, fmt.Errorf("provider returned no models")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
