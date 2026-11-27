package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"forge/internal/store"
)

func providerTestRequest(t *testing.T, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestProvidersCRUDAndMasking(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "sk-secret-123456"
	c.Providers.OpenRouter.BaseURL = "https://openrouter.ai/api/v1"
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(cfg, st, os.DirFS("."), "test").Handler())
	defer app.Close()

	resp, data := providerTestRequest(t, http.MethodGet, app.URL+"/api/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d: %s", resp.StatusCode, data)
	}
	if bytes.Contains(data, []byte("sk-secret-123456")) {
		t.Fatalf("raw API key leaked in provider list: %s", data)
	}
	var list struct {
		Providers []providerDTO `json:"providers"`
		ActiveID  string        `json:"active_id"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Providers) != 1 || list.Providers[0].ID != "openrouter" || list.ActiveID != "openrouter" {
		t.Fatalf("seeded providers = %+v", list)
	}
	if !list.Providers[0].APIKeySet || list.Providers[0].APIKeyHint == "" {
		t.Fatalf("seeded key metadata = %+v", list.Providers[0])
	}

	resp, data = providerTestRequest(t, http.MethodPost, app.URL+"/api/providers", map[string]any{
		"name":          "Ollama Local",
		"base_url":      "http://localhost:11434/v1",
		"default_model": "qwen2.5-coder:7b",
		"is_active":     false,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.StatusCode, data)
	}
	var created struct {
		Provider providerDTO `json:"provider"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Provider.ID != "ollama-local" || created.Provider.IsActive {
		t.Fatalf("created provider = %+v", created.Provider)
	}

	resp, data = providerTestRequest(t, http.MethodPut, app.URL+"/api/providers/ollama-local", map[string]any{
		"name":          "Ollama",
		"driver":        "openai_compatible",
		"base_url":      "http://localhost:11434/v1/",
		"api_key":       "__KEEP__",
		"default_model": "qwen2.5-coder:14b",
		"extra_headers": map[string]string{},
		"is_active":     true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d: %s", resp.StatusCode, data)
	}
	var updated struct {
		Provider providerDTO `json:"provider"`
	}
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Provider.IsActive || updated.Provider.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("updated provider = %+v", updated.Provider)
	}

	resp, data = providerTestRequest(t, http.MethodGet, app.URL+"/api/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if list.ActiveID != "ollama-local" {
		t.Fatalf("active_id = %q, want ollama-local", list.ActiveID)
	}

	resp, _ = providerTestRequest(t, http.MethodDelete, app.URL+"/api/providers/openrouter", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
}

func TestProviderTestEndpoint(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	app := httptest.NewServer(New(cfg, st, os.DirFS("."), "test").Handler())
	defer app.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`)
	}))
	defer upstream.Close()

	resp, data := providerTestRequest(t, http.MethodPost, app.URL+"/api/providers/test", map[string]any{
		"name":     "Inline",
		"base_url": upstream.URL + "/v1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test status = %d: %s", resp.StatusCode, data)
	}
	var result struct {
		OK              bool `json:"ok"`
		ModelsSupported bool `json:"models_supported"`
		Count           int  `json:"count"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.ModelsSupported || result.Count != 2 {
		t.Fatalf("test result = %+v (%s)", result, data)
	}

	noModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer noModels.Close()
	resp, data = providerTestRequest(t, http.MethodPost, app.URL+"/api/providers/test", map[string]any{
		"name":     "NoModels",
		"base_url": noModels.URL,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test status = %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.ModelsSupported {
		t.Fatalf("no-models result = %+v (%s)", result, data)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	resp, data = providerTestRequest(t, http.MethodPost, app.URL+"/api/providers/test", map[string]any{
		"name":     "Dead",
		"base_url": deadURL,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dead test status = %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatalf("dead endpoint should fail: %s", data)
	}
}

func TestModelsUsesRequestedProvider(t *testing.T) {
	cfg, st, _ := modelsFixture(t)

	modelsServer := func(model string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":%q,"context_length":4096}]}`, model)
		}))
	}
	first := modelsServer("first/model")
	defer first.Close()
	second := modelsServer("second/model")
	defer second.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = first.URL + "/v1"
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, os.DirFS("."), "test")
	if _, err := st.CreateProvider(store.ProviderProfile{
		ID: "second", Name: "Second", BaseURL: second.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(srv.Handler())
	defer app.Close()

	fetch := func(query string) (string, string) {
		resp, data := providerTestRequest(t, http.MethodGet, app.URL+"/api/models"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("models status = %d: %s", resp.StatusCode, data)
		}
		var payload struct {
			Models     []struct{ ID string } `json:"models"`
			Source     string                `json:"source"`
			ProviderID string                `json:"provider_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Models) != 1 {
			t.Fatalf("models = %+v", payload)
		}
		return payload.Models[0].ID, payload.ProviderID
	}

	if model, provider := fetch(""); model != "first/model" || provider != "openrouter" {
		t.Fatalf("active catalog = %s (%s)", model, provider)
	}
	if model, provider := fetch("?provider_id=second"); model != "second/model" || provider != "second" {
		t.Fatalf("requested catalog = %s (%s)", model, provider)
	}
}

func sseContentProvider(content string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"gen\",\"model\":\"mock/model\",\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", content)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestChatUsesPinnedAndActiveProvidersWithoutRestart(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(sseContentProvider("from-first", &firstHits))
	defer first.Close()
	second := httptest.NewServer(sseContentProvider("from-second", &secondHits))
	defer second.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = first.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	// Seed the legacy OpenRouter profile first, then add the second provider.
	srv := New(cfg, st, os.DirFS("."), "test")
	if _, err := st.CreateProvider(store.ProviderProfile{
		ID: "second", Name: "Second", BaseURL: second.URL, DefaultModel: "mock/model",
	}); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(projectDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject("P", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Pinned", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(srv.Handler())
	defer app.Close()

	resp, data := providerTestRequest(t, http.MethodPatch, fmt.Sprintf("%s/api/sessions/%d", app.URL, session.ID), map[string]any{
		"provider_id": "second",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d: %s", resp.StatusCode, data)
	}

	resp, data = providerTestRequest(t, http.MethodPost, app.URL+"/api/chat/stream", map[string]any{
		"session_id": session.ID,
		"content":    "hello",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("from-second")) || bytes.Contains(data, []byte("from-first")) {
		t.Fatalf("pinned chat used the wrong provider: %s", data)
	}
	if secondHits.Load() == 0 || firstHits.Load() != 0 {
		t.Fatalf("hits first=%d second=%d", firstHits.Load(), secondHits.Load())
	}

	// Make the second provider globally active. A session without an override
	// must use it on the next turn, without restarting the server.
	resp, data = providerTestRequest(t, http.MethodPut, app.URL+"/api/providers/second", map[string]any{
		"name":          "Second",
		"driver":        "openai_compatible",
		"base_url":      second.URL,
		"api_key":       "__KEEP__",
		"default_model": "mock/model",
		"extra_headers": map[string]string{},
		"is_active":     true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activate status = %d: %s", resp.StatusCode, data)
	}
	secondHits.Store(0)
	other, err := st.CreateSession(project.ID, "Global", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	resp, data = providerTestRequest(t, http.MethodPost, app.URL+"/api/chat/stream", map[string]any{
		"session_id": other.ID,
		"content":    "hello again",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("from-second")) || bytes.Contains(data, []byte("from-first")) {
		t.Fatalf("global switch did not apply: %s", data)
	}
	if secondHits.Load() == 0 || firstHits.Load() != 0 || strings.Contains(string(data), "error") {
		t.Fatalf("hits first=%d second=%d body=%s", firstHits.Load(), secondHits.Load(), data)
	}
}
