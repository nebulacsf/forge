package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		" https://host/v1/ ":                "https://host/v1",
		"https://host/v1/chat/completions":  "https://host/v1",
		"https://host/v1/chat/completions/": "https://host/v1",
		"http://localhost:11434/v1/models":  "http://localhost:11434/v1",
		"http://localhost:11434/v1/models/": "http://localhost:11434/v1",
		"https://openrouter.ai/api/v1":      "https://openrouter.ai/api/v1",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamChatGenericProvider(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotTitle string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotTitle = r.Header.Get("X-Title")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"gen-1","model":"local/model","choices":[{"delta":{"reasoning":"think"}}]}`,
			`{"id":"gen-1","model":"local/model","choices":[{"delta":{"content":"hello"}}]}`,
			`{"id":"gen-1","model":"local/model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_","arguments":"{\"pa"}}]}}]}`,
			`{"id":"gen-1","model":"local/model","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"th\":\"x\"}"}}]}}]}`,
			`{"id":"gen-1","model":"local/model","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(Profile{
		ID:      "local",
		Name:    "Local",
		BaseURL: srv.URL + "/v1/",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"X-Title": "Forge",
		},
	})
	var deltas []string
	res, err := client.StreamChat(context.Background(), ChatRequest{
		Model:    "local/model",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(d StreamDelta) error {
		deltas = append(deltas, d.Kind+":"+d.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotTitle != "Forge" {
		t.Errorf("X-Title = %q", gotTitle)
	}
	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"]; !ok {
		t.Error("stream_options missing from request")
	}
	if res.Content != "hello" || res.Reasoning != "think" {
		t.Errorf("content/reasoning = %q/%q", res.Content, res.Reasoning)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Function.Name != "read_file" ||
		res.ToolCalls[0].Function.Arguments != `{"path":"x"}` {
		t.Errorf("tool call = %+v", res.ToolCalls[0])
	}
	if res.Usage.PromptTokens != 3 || res.Usage.CompletionTokens != 4 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if strings.Join(deltas, ",") != "reasoning:think,content:hello" {
		t.Errorf("deltas = %v", deltas)
	}
}

func TestStreamChatOmitsAuthorizationWithoutKey(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(Profile{Name: "Local", BaseURL: srv.URL})
	res, err := client.StreamChat(context.Background(), ChatRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if sawAuth {
		t.Error("Authorization header was sent without an API key")
	}
	if res.Content != "ok" {
		t.Errorf("content = %q", res.Content)
	}
	if res.Usage.PromptTokens != 0 || res.Usage.Cost != 0 {
		t.Errorf("usage should be zero when omitted: %+v", res.Usage)
	}
}

func TestStreamChatRetriesWithoutStreamOptions(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			if !strings.Contains(string(body), "stream_options") {
				t.Errorf("first attempt should include stream_options: %s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"unknown field stream_options"}}`)
			return
		}
		if strings.Contains(string(body), "stream_options") {
			t.Errorf("retry should omit stream_options: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"retried\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(Profile{Name: "Local", BaseURL: srv.URL})
	res, err := client.StreamChat(context.Background(), ChatRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if res.Content != "retried" {
		t.Errorf("content = %q", res.Content)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestFetchModelsAcceptsBothEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"openai data", `{"data":[{"id":"a/model","name":"A","context_length":4096}]}`},
		{"local models", `{"models":[{"id":"a/model","name":"A","context_length":4096}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()

			client := New(Profile{Name: "Local", BaseURL: srv.URL + "/v1"})
			models, err := client.FetchModels(context.Background())
			if err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			if len(models) != 1 || models[0].ID != "a/model" || models[0].ContextLength != 4096 {
				t.Fatalf("models = %+v", models)
			}
		})
	}
}

func TestProbeReportsUnsupportedModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := New(Profile{Name: "Local", BaseURL: srv.URL})
	if _, err := client.FetchModels(context.Background()); !errors.Is(err, ErrModelsUnsupported) {
		t.Fatalf("FetchModels error = %v, want ErrModelsUnsupported", err)
	}
	models, supported, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if supported || len(models) != 0 {
		t.Errorf("Probe supported/models = %v/%v", supported, models)
	}
}

func TestNewProviderRejectsUnknownDriver(t *testing.T) {
	if _, err := NewProvider(Profile{Driver: "not-a-driver"}); err == nil {
		t.Fatal("expected an unsupported driver error")
	}
}
