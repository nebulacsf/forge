package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"forge/internal/config"
	"forge/internal/llm"
	"forge/internal/store"
)

func modelsFixture(t *testing.T) (*config.Manager, *store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FORGE_HOME", home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(home, "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st, home
}

func TestModelsEndpointUsesProviderCatalog(t *testing.T) {
	cfg, st, _ := modelsFixture(t)

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{"id": "vendor/model-a", "name": "Model A", "context_length": 123456},
				map[string]any{"id": "vendor/model-b", "name": "Model B", "context_length": 8192},
			},
		})
	}))
	defer upstream.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	fetch := func(query string) ([]llm.ModelInfo, string) {
		resp, err := http.Get(app.URL + "/api/models" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var payload struct {
			Models []llm.ModelInfo `json:"models"`
			Source string          `json:"source"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload.Models, payload.Source
	}

	models, source := fetch("")
	if source != "openrouter" {
		t.Errorf("source = %q, want openrouter", source)
	}
	if len(models) != 2 || models[0].ID != "vendor/model-a" || models[0].ContextLength != 123456 {
		t.Errorf("unexpected models: %+v", models)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}

	// Cached: no second upstream request.
	if _, source = fetch(""); source != "openrouter" {
		t.Errorf("source after cache = %q", source)
	}
	if hits.Load() != 1 {
		t.Errorf("cached call hit upstream %d times, want 1", hits.Load())
	}

	// Explicit refresh forces a new fetch.
	fetch("?refresh=1")
	if hits.Load() != 2 {
		t.Errorf("refresh hits = %d, want 2", hits.Load())
	}
}

func TestModelsEndpointFallsBackToBuiltin(t *testing.T) {
	cfg, st, _ := modelsFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"nope"}}`, http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	resp, err := http.Get(app.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with fallback", resp.StatusCode)
	}
	var payload struct {
		Models  []llm.ModelInfo `json:"models"`
		Source  string          `json:"source"`
		Warning string          `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != "builtin" {
		t.Errorf("source = %q, want builtin", payload.Source)
	}
	if len(payload.Models) == 0 {
		t.Error("expected built-in models")
	}
	if payload.Warning == "" {
		t.Error("expected a warning explaining the fallback")
	}
	found := false
	for _, m := range payload.Models {
		if m.ID == "anthropic/claude-sonnet-4.5" && m.ContextLength == 200000 {
			found = true
		}
	}
	if !found {
		t.Error("built-in catalog missing Claude context length")
	}
}
