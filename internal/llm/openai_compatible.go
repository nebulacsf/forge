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

// OpenAICompatibleProvider speaks the OpenAI chat-completions protocol over
// HTTP + SSE. It is intentionally free of provider-specific branches; all
// variation comes from the Profile.
type OpenAICompatibleProvider struct {
	profile Profile
	HTTP    *http.Client
}

var _ Provider = (*OpenAICompatibleProvider)(nil)

// New builds a provider from a profile. The profile is normalised here so the
// rest of the client can assume a clean base URL and a non-nil header map.
func New(p Profile) *OpenAICompatibleProvider {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Driver = strings.ToLower(strings.TrimSpace(p.Driver))
	if p.Driver == "" {
		p.Driver = DriverOpenAICompatible
	}
	p.BaseURL = normalizeBaseURL(p.BaseURL)
	p.APIKey = strings.TrimSpace(p.APIKey)
	p.DefaultModel = strings.TrimSpace(p.DefaultModel)
	if p.ExtraHeaders == nil {
		p.ExtraHeaders = map[string]string{}
	}
	return &OpenAICompatibleProvider{
		profile: p,
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

// Profile returns a copy of the client configuration. The caller may mutate the
// returned header map without affecting the client.
func (c *OpenAICompatibleProvider) Profile() Profile {
	p := c.profile
	p.ExtraHeaders = make(map[string]string, len(c.profile.ExtraHeaders))
	for k, v := range c.profile.ExtraHeaders {
		p.ExtraHeaders[k] = v
	}
	return p
}

func (c *OpenAICompatibleProvider) displayName() string {
	if name := strings.TrimSpace(c.profile.Name); name != "" {
		return name
	}
	if id := strings.TrimSpace(c.profile.ID); id != "" {
		return id
	}
	return "provider"
}

// normalizeBaseURL removes trailing slashes and a trailing endpoint suffix so
// both "https://host/v1", "https://host/v1/" and
// "https://host/v1/chat/completions" work.
func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	for _, suffix := range []string{"/chat/completions", "/models"} {
		if strings.HasSuffix(raw, suffix) {
			raw = strings.TrimSuffix(raw, suffix)
			raw = strings.TrimRight(raw, "/")
			break
		}
	}
	return raw
}

func (c *OpenAICompatibleProvider) endpoint(path string) string {
	return c.profile.BaseURL + path
}

func (c *OpenAICompatibleProvider) applyHeaders(req *http.Request, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if c.profile.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.profile.APIKey)
	}
	for name, value := range c.profile.ExtraHeaders {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		req.Header.Set(name, value)
	}
}

func (c *OpenAICompatibleProvider) post(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.applyHeaders(httpReq, true)
	return c.HTTP.Do(httpReq)
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
func (c *OpenAICompatibleProvider) StreamChat(ctx context.Context, req ChatRequest, onDelta func(StreamDelta) error) (*StreamResult, error) {
	if strings.TrimSpace(c.profile.BaseURL) == "" {
		return nil, fmt.Errorf("%s base URL is not configured", c.displayName())
	}
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	endpoint := c.endpoint("/chat/completions")
	callID := nextCallID()
	traceRequest(callID, endpoint, body)

	logx.Debug("provider request", "call", callID, "provider", c.displayName(),
		"url", endpoint, "model", req.Model, "messages", len(req.Messages), "tools", len(req.Tools))

	started := time.Now()
	resp, err := c.post(ctx, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.displayName(), err)
	}

	// A few local gateways reject unknown request fields. Retry once without
	// stream_options rather than forcing every endpoint to support it.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if strings.Contains(strings.ToLower(string(data)), "stream_options") {
			req.StreamOptions = nil
			body, err = json.Marshal(req)
			if err != nil {
				return nil, fmt.Errorf("encode request: %w", err)
			}
			traceRequest(callID, endpoint, body)
			resp, err = c.post(ctx, endpoint, body)
			if err != nil {
				return nil, fmt.Errorf("connect to %s: %w", c.displayName(), err)
			}
		} else {
			traceErrorBody(callID, resp.StatusCode, data)
			return nil, providerError(resp.StatusCode, data, c.displayName())
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		traceErrorBody(callID, resp.StatusCode, data)
		return nil, providerError(resp.StatusCode, data, c.displayName())
	}
	traceResponse(callID, resp.StatusCode, resp.Header)
	logx.Debug("provider response", "call", callID, "provider", c.displayName(),
		"status", resp.StatusCode, "latency", time.Since(started).Round(time.Millisecond).String())

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
				logx.LLMTrace("llm stream done", "call", callID, "provider", c.displayName())
				return io.EOF
			}
			traceChunk(callID, line)
			var chunk streamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// Keep streaming through malformed keep-alive or provider
				// extensions, but surface genuine decoding issues later if
				// nothing useful arrived.
				continue
			}
			if chunk.Error != nil && chunk.Error.Message != "" {
				return fmt.Errorf("%s: %s", c.displayName(), chunk.Error.Message)
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
	traceResult(callID, result)
	logx.Debug("provider stream complete", "call", callID, "provider", c.displayName(),
		"model", result.Model, "chunks", chunks,
		"content_chars", len(result.Content), "reasoning_chars", len(result.Reasoning),
		"tool_calls", len(result.ToolCalls), "prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens, "cost", result.Usage.Cost,
		"duration", time.Since(started).Round(time.Millisecond).String(),
		"error", streamErr)
	return result, streamErr
}

func providerError(status int, body []byte, name string) error {
	var apiErr apiError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		return fmt.Errorf("%s (%d): %s", name, status, apiErr.Error.Message)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("%s (%d): %s", name, status, msg)
}
