// Package server exposes Forge's local REST and SSE API plus the embedded UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forge/internal/config"
	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
	"forge/internal/tools"
)

const keepAPIKey = "__KEEP__"

var projectPalette = []string{
	"#6c8cff", "#22c55e", "#f59e0b", "#ec4899",
	"#06b6d4", "#a855f7", "#ef4444", "#14b8a6",
}

// modelsTTL controls how long a successful provider model catalog is cached.
const modelsTTL = 6 * time.Hour

type Server struct {
	cfg   *config.Manager
	store *store.Store
	web   fs.FS
	// runs owns the agent turns. They are independent of any HTTP connection,
	// so switching project or closing the browser no longer stops an agent.
	runs *runRegistry
	// version is the build stamp injected by main. It is read once at startup
	// and served to the UI so the footer can show what is running.
	version string

	modelsMu    sync.Mutex
	modelsCache map[string]modelCacheEntry
	// fallbackMu guards the OpenRouter catalog used to complete context windows
	// the active provider does not publish. It has its own entry because
	// modelsCache is held while a provider request is in flight.
	fallbackMu      sync.Mutex
	fallbackEntry   modelCacheEntry
	fallbackWarming atomic.Bool
}

// modelCacheEntry is the per-provider model catalog cache. Providers can be
// switched without a restart, so a single global list is not enough.
type modelCacheEntry struct {
	models    []llm.ModelInfo
	source    string
	warning   string
	fetchedAt time.Time
}

func New(cfg *config.Manager, st *store.Store, web fs.FS, version string) *Server {
	s := &Server{
		cfg:         cfg,
		store:       st,
		web:         web,
		runs:        newRunRegistry(),
		version:     version,
		modelsCache: map[string]modelCacheEntry{},
	}
	s.seedProviders()
	return s
}

// Handler builds the complete HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("GET /api/models", s.handleListModels)

	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/providers", s.handleCreateProvider)
	mux.HandleFunc("POST /api/providers/test", s.handleTestProvider)
	mux.HandleFunc("PUT /api/providers/{id}", s.handleUpdateProvider)
	mux.HandleFunc("DELETE /api/providers/{id}", s.handleDeleteProvider)

	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.handlePatchProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)
	mux.HandleFunc("GET /api/projects/{id}/files", s.handleListProjectFiles)
	mux.HandleFunc("GET /api/projects/{id}/git", s.handleProjectGit)
	mux.HandleFunc("GET /api/projects/{id}/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/projects/{id}/sessions", s.handleCreateSession)

	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/compact", s.handleCompactSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleListMessages)
	mux.HandleFunc("GET /api/sessions/{id}/checklist", s.handleListChecklist)
	mux.HandleFunc("DELETE /api/sessions/{id}/checklist", s.handleClearChecklist)
	mux.HandleFunc("GET /api/sessions/{id}/questions", s.handleListQuestions)
	mux.HandleFunc("POST /api/sessions/{id}/questions/{qid}/answers", s.handleAnswerQuestions)

	// Parallel agents: a run survives the browser. These endpoints let the UI
	// re-attach to it, stop it explicitly, and know which chats are busy.
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleResumeSessionStream)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.handleStopSession)
	mux.HandleFunc("GET /api/sessions/{id}/run", s.handleSessionRun)
	mux.HandleFunc("POST /api/sessions/{id}/run/dismiss", s.handleDismissRun)
	mux.HandleFunc("GET /api/runs", s.handleListRuns)

	mux.HandleFunc("POST /api/checklist", s.handleCreateChecklist)
	mux.HandleFunc("PATCH /api/checklist/{id}", s.handlePatchChecklist)
	mux.HandleFunc("DELETE /api/checklist/{id}", s.handleDeleteChecklist)

	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)

	fileServer := http.FileServer(http.FS(s.web))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vendored libraries are pinned by the ?v= query in index.html, so they
		// can be cached aggressively. Everything else (index.html, app.js,
		// app.css) stays revalidated so a rebuilt binary or a dev edit is picked
		// up immediately.
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))

	return loggingMiddleware(recoverMiddleware(securityHeaders(mux)))
}

// statusRecorder captures the response status and size for request logging.
// Flush/Unwrap keep SSE streaming working through the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
			"bytes", rec.bytes,
		}
		switch {
		case status >= 500:
			logx.Error("http request", attrs...)
		case status >= 400:
			logx.Warn("http request", attrs...)
		default:
			logx.Debug("http request", attrs...)
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logx.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", rec)
				if strings.HasPrefix(r.URL.Path, "/api/") {
					apiError(w, http.StatusInternalServerError, "internal server error")
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logx.Error("write json response", "error", err)
	}
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "••••••••"
	}
	return key[:4] + "••••••••" + key[len(key)-4:]
}

func (s *Server) publicConfig() map[string]any {
	cfg := s.cfg.Get()
	set := cfg.Providers.OpenRouter.APIKey != ""
	if p, err := s.activeProvider(); err == nil {
		set = p.APIKey != ""
	}
	cfg.Providers.OpenRouter.APIKey = maskAPIKey(cfg.Providers.OpenRouter.APIKey)
	logPath, _ := config.LogPath(cfg)
	return map[string]any{
		"config":      cfg,
		"path":        s.cfg.Path(),
		"api_key_set": set,
		"log_path":    logPath,
		"version":     s.version,
		"debug":       logx.DebugEnabled(),
		"llm_debug":   logx.LLMEnabled(),
	}
}

// handleListModels proxies the selected provider's model catalog so the UI can
// use real context-window sizes. It always succeeds, falling back to a useful
// list instead of failing the page.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	providerID := strings.TrimSpace(r.URL.Query().Get("provider_id"))
	models, source, warning, resolved := s.modelCatalog(r.Context(), providerID, r.URL.Query().Get("refresh") == "1")
	resp := map[string]any{"models": models, "source": source, "provider_id": resolved}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// modelCatalog returns the provider catalog plus whatever context windows Forge
// could complete from the OpenRouter catalog. Completion happens outside
// modelsMu so a slow public catalog never blocks the provider actually in use.
func (s *Server) modelCatalog(ctx context.Context, providerID string, refresh bool) ([]llm.ModelInfo, string, string, string) {
	models, source, warning, resolved := s.providerCatalog(ctx, providerID, refresh)
	return s.withFallbackContext(models), source, warning, resolved
}

// providerCatalog proxies the selected provider's own /models endpoint.
func (s *Server) providerCatalog(ctx context.Context, providerID string, refresh bool) ([]llm.ModelInfo, string, string, string) {
	profile, err := s.providerByID(providerID)
	if err != nil {
		// Fall back to the built-in catalog when even provider resolution fails.
		return llm.BuiltinModels(), "builtin", err.Error(), providerID
	}
	cacheKey := profile.ID
	if cacheKey == "" {
		cacheKey = "default"
	}

	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if s.modelsCache == nil {
		s.modelsCache = map[string]modelCacheEntry{}
	}

	if entry, ok := s.modelsCache[cacheKey]; ok {
		ttl := modelsTTL
		if entry.source == "builtin" {
			ttl = 2 * time.Minute
		}
		if !refresh && time.Since(entry.fetchedAt) < ttl {
			return entry.models, entry.source, entry.warning, profile.ID
		}
	}

	client, err := s.providerClient(profile)
	if err != nil {
		entry := modelCacheEntry{
			models:    fallbackModels(profile),
			source:    "builtin",
			warning:   err.Error(),
			fetchedAt: time.Now(),
		}
		s.modelsCache[cacheKey] = entry
		return entry.models, entry.source, entry.warning, profile.ID
	}
	models, fetchErr := client.FetchModels(ctx)
	if fetchErr != nil {
		if existing, ok := s.modelsCache[cacheKey]; !ok || existing.source != "builtin" {
			logx.Warn("model catalog unavailable, using fallback list",
				"provider", profile.ID, "base_url", profile.BaseURL, "error", fetchErr)
		}
		entry := modelCacheEntry{
			models:    fallbackModels(profile),
			source:    "builtin",
			warning:   fetchErr.Error(),
			fetchedAt: time.Now(),
		}
		s.modelsCache[cacheKey] = entry
		return entry.models, entry.source, entry.warning, profile.ID
	}
	logx.Debug("model catalog refreshed", "provider", profile.ID, "models", len(models))
	entry := modelCacheEntry{models: models, source: profile.ID, fetchedAt: time.Now()}
	if entry.source == "" {
		entry.source = "provider"
	}
	s.modelsCache[cacheKey] = entry
	return entry.models, entry.source, entry.warning, profile.ID
}

// fallbackModels keeps local providers usable when /models is unavailable.
// OpenRouter keeps the shipped built-in list; other providers expose their
// configured default model (the model input remains free-text).
func fallbackModels(p store.ProviderProfile) []llm.ModelInfo {
	if strings.EqualFold(p.ID, "openrouter") || strings.Contains(strings.ToLower(p.BaseURL), "openrouter.ai") {
		return llm.BuiltinModels()
	}
	if model := strings.TrimSpace(p.DefaultModel); model != "" {
		return []llm.ModelInfo{{ID: model, Name: model}}
	}
	return []llm.ModelInfo{}
}

func (s *Server) providerByID(id string) (store.ProviderProfile, error) {
	if strings.TrimSpace(id) != "" {
		p, err := s.store.GetProvider(strings.TrimSpace(id))
		if err == nil {
			return p, nil
		}
		return s.activeProvider()
	}
	return s.activeProvider()
}

// modelContextLength uses the cached catalog for the active provider when
// available, then the OpenRouter catalog, then the built-in table. A catalog
// entry without a window never short-circuits the chain: reporting 0 because the
// provider stayed silent would leave the pruning budget with the token cap only.
func (s *Server) modelContextLength(providerID, model string) int {
	model = strings.TrimSpace(model)
	if model == "" {
		return 0
	}
	if providerID != "" {
		s.modelsMu.Lock()
		entry, ok := s.modelsCache[providerID]
		s.modelsMu.Unlock()
		if ok {
			if length := llm.ContextLengthFor(entry.models, model); length > 0 {
				return length
			}
		}
	}
	// The turn path never waits on the public catalog: it uses whatever is
	// already cached, and /api/models warms it for the next round.
	if length := llm.ContextLengthFor(s.cachedFallbackCatalog(), model); length > 0 {
		return length
	}
	return llm.BuiltinContextLength(model)
}

// withFallbackContext fills in context windows the provider did not publish so
// the UI can show a real denominator. Models are never added or removed: only the
// ContextLength of an existing row is completed.
func (s *Server) withFallbackContext(models []llm.ModelInfo) []llm.ModelInfo {
	if len(models) == 0 || !s.cfg.Get().Defaults.OpenRouterCatalogEnabled() {
		return models
	}
	// The public catalog is only consulted when this provider actually left a
	// window empty. A provider that publishes every window costs no extra request.
	missing := false
	for _, m := range models {
		if m.ContextLength <= 0 {
			missing = true
			break
		}
	}
	if !missing {
		return models
	}
	catalog := s.fallbackCatalog()
	if len(catalog) == 0 {
		return models
	}
	out := make([]llm.ModelInfo, len(models))
	copy(out, models)
	for i := range out {
		if out[i].ContextLength > 0 {
			continue
		}
		if length := llm.ContextLengthFor(catalog, out[i].ID); length > 0 {
			out[i].ContextLength = length
		}
	}
	return out
}

// fallbackFetchTimeout bounds the single blocking fetch of the public catalog.
// Refreshes after that happen in the background, so only the very first request
// after a restart can wait, and only briefly.
const fallbackFetchTimeout = 3 * time.Second

// fallbackBaseURL is the public catalog endpoint. It reuses the legacy OpenRouter
// base URL so a test or a mirror can point it elsewhere, and falls back to the
// shipped default when the section is empty.
func (s *Server) fallbackBaseURL() string {
	if base := strings.TrimSpace(s.cfg.Get().Providers.OpenRouter.BaseURL); base != "" {
		return base
	}
	return config.Default().Providers.OpenRouter.BaseURL
}

// cachedFallbackCatalog is fallbackCatalog without the network: it returns the
// cached list or nothing, so a turn never blocks on an unrelated endpoint.
func (s *Server) cachedFallbackCatalog() []llm.ModelInfo {
	if !s.cfg.Get().Defaults.OpenRouterCatalogEnabled() {
		return nil
	}
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	return s.fallbackEntry.models
}

// fallbackCatalog returns the OpenRouter model catalog used to complete context
// windows. It is cached separately from modelsCache and never requires an API
// key: the list endpoint is public and only metadata is read from it.
func (s *Server) fallbackCatalog() []llm.ModelInfo {
	if !s.cfg.Get().Defaults.OpenRouterCatalogEnabled() {
		return nil
	}
	s.fallbackMu.Lock()
	entry := s.fallbackEntry
	s.fallbackMu.Unlock()
	if len(entry.models) > 0 {
		if time.Since(entry.fetchedAt) < modelsTTL {
			return entry.models
		}
		// Serve the stale list while one refresh runs, so a long-lived process
		// never pays the round trip inside a request.
		if s.fallbackWarming.CompareAndSwap(false, true) {
			go func() {
				defer s.fallbackWarming.Store(false)
				s.refreshFallbackCatalog(context.Background(), modelsTTL)
			}()
		}
		return entry.models
	}
	// First use: fetch once with a short deadline so the first gauge can already
	// show a real window. Every later refresh stays off the request path.
	if s.fallbackWarming.CompareAndSwap(false, true) {
		defer s.fallbackWarming.Store(false)
		s.refreshFallbackCatalog(context.Background(), fallbackFetchTimeout)
		s.fallbackMu.Lock()
		entry = s.fallbackEntry
		s.fallbackMu.Unlock()
	}
	return entry.models
}

// refreshFallbackCatalog fetches the public OpenRouter catalog. Failures keep the
// built-in table in place, so a machine without network loses nothing it had.
func (s *Server) refreshFallbackCatalog(ctx context.Context, timeout time.Duration) {
	client, err := llm.NewProvider(llm.Profile{
		ID:      "openrouter-catalog",
		Name:    "OpenRouter catalog",
		BaseURL: s.fallbackBaseURL(),
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	models, err := client.FetchModels(ctx)
	if err != nil {
		logx.Debug("openrouter context catalog unavailable", "error", err)
		s.fallbackMu.Lock()
		s.fallbackEntry.fetchedAt = time.Now()
		s.fallbackMu.Unlock()
		return
	}
	logx.Debug("openrouter context catalog refreshed", "models", len(models))
	s.fallbackMu.Lock()
	s.fallbackEntry = modelCacheEntry{models: models, source: "openrouter", fetchedAt: time.Now()}
	s.fallbackMu.Unlock()
}

func (s *Server) invalidateModels() {
	s.modelsMu.Lock()
	s.modelsCache = map[string]modelCacheEntry{}
	s.modelsMu.Unlock()
	s.fallbackMu.Lock()
	s.fallbackEntry = modelCacheEntry{}
	s.fallbackMu.Unlock()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.publicConfig())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var incoming config.Config
	if err := decodeJSON(w, r, &incoming); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	current := s.cfg.Get()
	if incoming.Providers.OpenRouter.APIKey == "" ||
		incoming.Providers.OpenRouter.APIKey == keepAPIKey ||
		strings.Contains(incoming.Providers.OpenRouter.APIKey, "••") {
		incoming.Providers.OpenRouter.APIKey = current.Providers.OpenRouter.APIKey
	}
	if err := s.cfg.Update(incoming); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := s.cfg.Get()
	logx.ApplyDebug(next.Server.Debug || next.Server.DebugLLM)
	logx.ApplyLLM(next.Server.DebugLLM)
	if current.Providers.OpenRouter.BaseURL != next.Providers.OpenRouter.BaseURL ||
		current.Providers.OpenRouter.APIKey != next.Providers.OpenRouter.APIKey {
		s.invalidateModels()
	}
	logx.Info("configuration updated", "debug", logx.DebugEnabled(), "model", next.Defaults.Model)
	writeJSON(w, http.StatusOK, s.publicConfig())
}

// ---------------------------------------------------------------------------
// projects

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Name      string `json:"name"`
		IconColor string `json:"icon_color"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	abs, err := normalizeProjectPath(body.Path)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = filepath.Base(abs)
	}
	color := strings.TrimSpace(body.IconColor)
	if !validColor(color) {
		color = colorForPath(abs)
	}
	project, err := s.store.CreateProject(name, abs, color)
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	go s.warmRepoOutline(project)
	projects, _ := s.store.ListProjects()
	writeJSON(w, http.StatusCreated, map[string]any{"project": project, "projects": projects})
}

func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Path      *string `json:"path"`
		Name      *string `json:"name"`
		IconColor *string `json:"icon_color"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.IconColor != nil {
		color := strings.TrimSpace(*body.IconColor)
		if !validColor(color) {
			apiError(w, http.StatusBadRequest, "color must be a hex value like #6c8cff")
			return
		}
		body.IconColor = &color
	}
	current, err := s.store.GetProject(id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	moved := false
	if body.Path != nil {
		abs, err := normalizeProjectPath(*body.Path)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		moved = abs != current.Path
		body.Path = &abs
	}
	// An empty display name falls back to the directory name, like create does.
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			target := current.Path
			if body.Path != nil {
				target = *body.Path
			}
			name = filepath.Base(target)
		}
		body.Name = &name
	}
	project, err := s.store.UpdateProject(id, body.Name, body.Path, body.IconColor)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	if moved {
		go s.warmRepoOutline(project)
	}
	logx.Info("project updated", "project", project.ID, "path", project.Path, "moved", moved)
	projects, _ := s.store.ListProjects()
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "projects": projects})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.DeleteProject(id); errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "project not found")
		return
	} else if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Info("project deleted", "project", id)
	projects, _ := s.store.ListProjects()
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// handleListProjectFiles lists the project files available to the composer's
// "@" attachment picker. The tools.FileSet decides membership, so tracked
// dotfiles are listed and git-ignored paths are not; symlinks and vendor/build
// directories are still skipped. The query parameter q filters by
// case-insensitive substring.
func (s *Server) handleListProjectFiles(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := s.store.GetProject(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	files, err := tools.ProjectFiles(project.Path, 0)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))); q != "" {
		filtered := make([]tools.ProjectFile, 0, len(files))
		for _, f := range files {
			if strings.Contains(strings.ToLower(f.Path), q) {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID,
		"root":       project.Path,
		"files":      files,
	})
}

// warmRepoOutline builds the local repository map in the background so the
// first chat turn is fast. Failures are non-fatal; the turn will retry.
func (s *Server) warmRepoOutline(project store.Project) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	outline, err := tools.EnsureProjectIndex(ctx, s.store, project.ID, project.Path)
	if err != nil {
		logx.Warn("project index warmup failed", "project", project.ID, "error", err)
		return
	}
	logx.Debug("project index warmed", "project", project.ID,
		"chars", len(outline), "duration", time.Since(started).Round(time.Millisecond).String())
}

func normalizeProjectPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot open directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

func validColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func colorForPath(p string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(p))
	return projectPalette[int(h.Sum32())%len(projectPalette)]
}

// ---------------------------------------------------------------------------
// sessions

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetProject(projectID); err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	openOnly := r.URL.Query().Get("all") != "1"
	sessions, err := s.store.ListSessions(projectID, openOnly)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := s.store.GetProject(projectID)
	if err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	var body struct {
		Title      string `json:"title"`
		Model      string `json:"model"`
		Mode       string `json:"mode"`
		ProviderID string `json:"provider_id"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	body.ProviderID = strings.TrimSpace(body.ProviderID)
	var provider store.ProviderProfile
	var providerErr error
	if body.ProviderID != "" {
		provider, providerErr = s.store.GetProvider(body.ProviderID)
		if errors.Is(providerErr, store.ErrNotFound) {
			apiError(w, http.StatusBadRequest, "provider not found")
			return
		}
	} else {
		provider, providerErr = s.activeProvider()
	}
	if providerErr != nil {
		apiError(w, http.StatusBadRequest, providerErr.Error())
		return
	}
	cfg := s.cfg.Get()
	if strings.TrimSpace(body.Model) == "" {
		body.Model = s.defaultModelFor(provider)
	}
	if strings.TrimSpace(body.Model) == "" {
		body.Model = cfg.Defaults.Model
	}
	if strings.TrimSpace(body.Mode) == "" {
		body.Mode = cfg.Defaults.Mode
	}
	session, err := s.store.CreateSession(project.ID, body.Title, body.Model, body.Mode)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.ProviderID != "" {
		if pinned, err := s.store.SetSessionProvider(session.ID, body.ProviderID); err == nil {
			session = pinned
		} else {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	_ = s.store.TouchProject(project.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"session": session})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	session, err := s.store.GetSession(id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	project, err := s.store.GetProject(session.ProjectID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	messages, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	checklist, err := s.store.ListChecklist(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	questionBatches, err := s.store.ListQuestionBatches(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Compactions are returned alongside the full transcript: the UI renders
	// every message and shows each summary card where the context boundary is.
	compactions, err := s.store.ListCompactions(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.TouchProject(project.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"session":          session,
		"project":          project,
		"messages":         messageDTOs(messages),
		"compactions":      compactions,
		"checklist":        checklist,
		"question_batches": questionBatches,
	})
}

func messageDTOs(messages []store.Message) []messageDTO {
	dtos := make([]messageDTO, 0, len(messages))
	for _, m := range messages {
		raw := json.RawMessage(m.ToolCalls)
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("[]")
		}
		dtos = append(dtos, messageDTO{
			ID: m.ID, Role: m.Role, Reasoning: m.Reasoning, Content: m.Content,
			ToolCalls: raw, DurationMS: m.DurationMS, CreatedAt: m.CreatedAt,
		})
	}
	return dtos
}

type messageDTO struct {
	ID         int64           `json:"id"`
	Role       string          `json:"role"`
	Reasoning  string          `json:"reasoning"`
	Content    string          `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	DurationMS int64           `json:"duration_ms"`
	CreatedAt  string          `json:"created_at"`
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Title      *string `json:"title"`
		Model      *string `json:"model"`
		Mode       *string `json:"mode"`
		IsOpen     *bool   `json:"is_open"`
		ProviderID *string `json:"provider_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ProviderID != nil {
		id := strings.TrimSpace(*body.ProviderID)
		body.ProviderID = &id
		if id != "" {
			if _, err := s.store.GetProvider(id); errors.Is(err, store.ErrNotFound) {
				apiError(w, http.StatusBadRequest, "provider not found")
				return
			} else if err != nil {
				apiError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) == "" {
		apiError(w, http.StatusBadRequest, "title cannot be empty")
		return
	}
	if body.Model != nil && strings.TrimSpace(*body.Model) == "" {
		apiError(w, http.StatusBadRequest, "model cannot be empty")
		return
	}
	// A run captures its mode when it starts, because the mode decides which
	// tools are exposed and whether they may write. Changing it mid-run would
	// silently desynchronize the agent from the UI.
	if body.Mode != nil || (body.IsOpen != nil && !*body.IsOpen) {
		if rn := s.runs.liveRun(id); rn != nil {
			if body.Mode != nil {
				apiError(w, http.StatusConflict,
					"this chat has a running agent; stop it before changing the mode")
				return
			}
			apiError(w, http.StatusConflict,
				"this chat has a running agent; stop it before closing the tab")
			return
		}
	}
	session, err := s.store.UpdateSession(id, body.Title, body.Model, body.Mode, body.IsOpen)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ProviderID != nil {
		if updated, err := s.store.SetSessionProvider(id, *body.ProviderID); err == nil {
			session = updated
		} else if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusBadRequest, "provider not found")
			return
		} else {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Deleting a chat must not race with its own agent: stop the run and let it
	// persist what it produced before the rows disappear.
	s.runs.stopAndWait(id, 15*time.Second)
	if err := s.store.DeleteSession(id); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The chat is gone, so its finished-run entry in the agents modal must not
	// linger and offer to open a missing chat.
	s.runs.forget(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	messages, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]messageDTO, 0, len(messages))
	for _, m := range messages {
		raw := json.RawMessage(m.ToolCalls)
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("[]")
		}
		dtos = append(dtos, messageDTO{ID: m.ID, Role: m.Role, Reasoning: m.Reasoning, Content: m.Content, ToolCalls: raw, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": dtos})
}

// ---------------------------------------------------------------------------
// checklist

func (s *Server) checklistResponse(w http.ResponseWriter, sessionID int64) {
	items, err := s.store.ListChecklist(sessionID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checklist": items})
}

func (s *Server) handleListChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checklistResponse(w, id)
}

func (s *Server) handleCreateChecklist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID int64  `json:"session_id"`
		TaskText  string `json:"task_text"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionID <= 0 {
		apiError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if _, err := s.store.AddChecklist(body.SessionID, body.TaskText); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checklistResponse(w, body.SessionID)
}

func (s *Server) handlePatchChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		SessionID int64   `json:"session_id"`
		TaskText  *string `json:"task_text"`
		IsDone    *bool   `json:"is_done"`
		Position  *int    `json:"position"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionID <= 0 {
		apiError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := s.store.UpdateChecklist(body.SessionID, id, body.TaskText, body.IsDone, body.Position); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "checklist item not found")
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, body.SessionID)
}

func (s *Server) handleDeleteChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID, _ := strconv.ParseInt(r.URL.Query().Get("session_id"), 10, 64)
	if sessionID <= 0 {
		var body struct {
			SessionID int64 `json:"session_id"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, "session_id is required")
			return
		}
		sessionID = body.SessionID
	}
	if err := s.store.DeleteChecklist(sessionID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "checklist item not found")
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, sessionID)
}

func (s *Server) handleClearChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.ClearChecklist(id); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, id)
}
