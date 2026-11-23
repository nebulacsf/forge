package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/logx"
)

func initTrace(t *testing.T, enabled bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.log")
	if err := logx.Init(logx.Options{LLMDebug: enabled, Debug: enabled, FilePath: path}); err != nil {
		t.Fatalf("logx.Init: %v", err)
	}
	t.Cleanup(func() {
		_ = logx.Close()
		logx.ApplyLLM(false)
		logx.ApplyDebug(false)
	})
	return path
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	if err := logx.Close(); err != nil {
		t.Fatalf("logx.Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

// sseStream writes a reasoning delta, a content delta and a fragmented tool call
// followed by a usage chunk and [DONE].
func sseStream(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-trace-1")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"gen-1","model":"test/model","choices":[{"delta":{"reasoning":"thinking-trace"}}]}`,
			`{"id":"gen-1","model":"test/model","choices":[{"delta":{"content":"answer-trace"}}]}`,
			`{"id":"gen-1","model":"test/model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"trace-target.go\"}"}}]}}]}`,
			`{"id":"gen-1","model":"test/model","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func TestStreamChatTracesFullPayloads(t *testing.T) {
	path := initTrace(t, true)

	srv := httptest.NewServer(sseStream(t))
	defer srv.Close()

	client := New(Profile{Name: "OpenRouter", APIKey: "sk-secret-trace-key", BaseURL: srv.URL})
	req := ChatRequest{
		Model: "test/model",
		Messages: []Message{
			{Role: "system", Content: "system-prompt-trace"},
			{Role: "user", Content: "user-message-trace"},
			{Role: "tool", Content: "tool-result-trace", ToolCallID: "call_prev"},
		},
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunctionSpec{
				Name:        "read_file",
				Description: "read a file",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	}
	var streamed strings.Builder
	res, err := client.StreamChat(context.Background(), req, func(d Delta) error {
		streamed.WriteString(d.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if res == nil || res.Content != "answer-trace" || res.Reasoning != "thinking-trace" {
		t.Fatalf("unexpected result: %+v", res)
	}

	text := readLog(t, path)
	wants := []string{
		"level=LLM",
		// request body: the exact payload sent to the model
		"llm request call-",
		"system-prompt-trace",
		"user-message-trace",
		"tool-result-trace",
		"read_file",
		`"stream": true`,
		`"stream_options"`,
		// raw stream chunks and the assembled response
		"llm stream chunk",
		"answer-trace",
		"thinking-trace",
		"trace-target.go",
		"llm response call-",
		"req-trace-1",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("log missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sk-secret-trace-key") {
		t.Errorf("API key leaked into the log:\n%s", text)
	}
}

func TestStreamChatTracesProviderErrorBody(t *testing.T) {
	path := initTrace(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate-limited-trace","code":429}}`)
	}))
	defer srv.Close()

	client := New(Profile{Name: "OpenRouter", APIKey: "sk-secret-trace-key", BaseURL: srv.URL})
	_, err := client.StreamChat(context.Background(), ChatRequest{Model: "test/model"}, nil)
	if err == nil {
		t.Fatal("expected a provider error")
	}

	text := readLog(t, path)
	if !strings.Contains(text, "llm error body") || !strings.Contains(text, "rate-limited-trace") {
		t.Errorf("provider error body was not traced:\n%s", text)
	}
	if strings.Contains(text, "sk-secret-trace-key") {
		t.Errorf("API key leaked into the log:\n%s", text)
	}
}

func TestStreamChatSilentWithoutLLMTrace(t *testing.T) {
	path := initTrace(t, false)

	srv := httptest.NewServer(sseStream(t))
	defer srv.Close()

	client := New(Profile{Name: "OpenRouter", APIKey: "sk-secret-trace-key", BaseURL: srv.URL})
	if _, err := client.StreamChat(context.Background(), ChatRequest{
		Model:    "test/model",
		Messages: []Message{{Role: "user", Content: "user-message-trace"}},
	}, nil); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	if err := logx.Close(); err != nil {
		t.Fatalf("logx.Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(data), "user-message-trace") || strings.Contains(string(data), "llm request call-") {
		t.Errorf("payloads traced while --debug-llm was off:\n%s", data)
	}
}
