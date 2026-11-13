// Package llm implements a streaming OpenRouter (OpenAI-compatible) client.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"forge/internal/logx"
)

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
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Usage mirrors OpenRouter's usage object.
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

type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func New(apiKey, baseURL string) *Client {
	return &Client{
		APIKey:  strings.TrimSpace(apiKey),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 180 * time.Second,
			},
		},
	}
}

type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// StreamChat sends a streaming completion request and invokes onDelta for each
// text fragment. It blocks until the stream finishes or ctx is cancelled.
func (c *Client) StreamChat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*StreamResult, error) {
	if c.APIKey == "" {
		return nil, errors.New("OpenRouter API key is not configured")
	}
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("HTTP-Referer", "https://github.com/forge-harness/forge")
	httpReq.Header.Set("X-Title", "Forge")

	logx.Debug("provider request", "url", c.BaseURL+"/chat/completions", "model", req.Model,
		"messages", len(req.Messages), "tools", len(req.Tools))

	started := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("connect to OpenRouter: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, providerError(resp.StatusCode, data)
	}
	logx.Debug("provider response", "status", resp.StatusCode, "latency", time.Since(started).Round(time.Millisecond).String())

	result := &StreamResult{}
	chunks := 0
	type partialTool struct {
		call ToolCall
	}
	tools := map[int]*partialTool{}
	order := []int{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var dataLines []string
	process := func() error {
		defer func() { dataLines = dataLines[:0] }()
		for _, line := range dataLines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "[DONE]" {
				return io.EOF
			}
			var chunk streamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// Keep streaming through malformed keep-alive or provider
				// extensions, but surface genuine decoding issues later if
				// nothing useful arrived.
				continue
			}
			if chunk.Error != nil && chunk.Error.Message != "" {
				return fmt.Errorf("OpenRouter: %s", chunk.Error.Message)
			}
			chunks++
			if chunk.ID != "" {
				result.ID = chunk.ID
			}
			if chunk.Model != "" {
				result.Model = chunk.Model
			}
			if chunk.Usage != nil {
				result.Usage = *chunk.Usage
			}
			for _, choice := range chunk.Choices {
				d := choice.Delta
				if d.ReasoningContent != "" || d.Reasoning != "" {
					text := d.ReasoningContent
					if text == "" {
						text = d.Reasoning
					}
					result.Reasoning += text
					if onDelta != nil {
						if err := onDelta(Delta{Kind: "reasoning", Text: text}); err != nil {
							return err
						}
					}
				}
				if d.Content != "" {
					result.Content += d.Content
					if onDelta != nil {
						if err := onDelta(Delta{Kind: "content", Text: d.Content}); err != nil {
							return err
						}
					}
				}
				for _, tc := range d.ToolCalls {
					p, ok := tools[tc.Index]
					if !ok {
						p = &partialTool{call: ToolCall{Type: "function"}}
						tools[tc.Index] = p
						order = append(order, tc.Index)
					}
					if tc.ID != "" {
						p.call.ID = tc.ID
					}
					if tc.Type != "" {
						p.call.Type = tc.Type
					}
					if tc.Function.Name != "" {
						p.call.Function.Name += tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						p.call.Function.Arguments += tc.Function.Arguments
					}
				}
			}
		}
		return nil
	}

	var streamErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) > 0 {
				if err := process(); err != nil {
					if errors.Is(err, io.EOF) {
						streamErr = nil
						break
					}
					streamErr = err
					break
				}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / keep-alive
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			dataLines = append(dataLines, data)
			continue
		}
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
	}
	if streamErr == nil && errors.Is(ctx.Err(), context.Canceled) {
		streamErr = context.Canceled
	}
	if streamErr == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		streamErr = context.DeadlineExceeded
	}
	if streamErr == nil {
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			streamErr = fmt.Errorf("read stream: %w", err)
		}
	}
	if len(dataLines) > 0 && streamErr == nil {
		if err := process(); err != nil && !errors.Is(err, io.EOF) {
			streamErr = err
		}
	}

	for i, idx := range order {
		call := tools[idx].call
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
		}
		if call.Type == "" {
			call.Type = "function"
		}
		result.ToolCalls = append(result.ToolCalls, call)
	}
	logx.Debug("provider stream complete", "model", result.Model, "chunks", chunks,
		"content_chars", len(result.Content), "reasoning_chars", len(result.Reasoning),
		"tool_calls", len(result.ToolCalls), "prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens, "cost", result.Usage.Cost,
		"duration", time.Since(started).Round(time.Millisecond).String(),
		"error", streamErr)
	return result, streamErr
}

func providerError(status int, body []byte) error {
	var apiErr apiError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		return fmt.Errorf("OpenRouter (%d): %s", status, apiErr.Error.Message)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("OpenRouter (%d): %s", status, msg)
}
