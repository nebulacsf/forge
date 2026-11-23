package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
)

const providerTestTimeout = 12 * time.Second

// providerDTO is the browser-facing shape. It never carries the raw key.
type providerDTO struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Driver       string            `json:"driver"`
	BaseURL      string            `json:"base_url"`
	DefaultModel string            `json:"default_model"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	IsActive     bool              `json:"is_active"`
	APIKeySet    bool              `json:"api_key_set"`
	APIKeyHint   string            `json:"api_key_hint,omitempty"`
	CreatedAt    string            `json:"created_at"`
}

type providerBody struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Driver       string            `json:"driver"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	DefaultModel string            `json:"default_model"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	IsActive     *bool             `json:"is_active"`
}

func (s *Server) seedProviders() {
	cfg := s.cfg.Get()
	key := firstNonEmpty(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("FORGE_OPENROUTER_API_KEY"),
		cfg.Providers.OpenRouter.APIKey)
	base := firstNonEmpty(os.Getenv("OPENROUTER_BASE_URL"), os.Getenv("FORGE_OPENROUTER_BASE_URL"),
		cfg.Providers.OpenRouter.BaseURL, "https://openrouter.ai/api/v1")
	profile := store.ProviderProfile{
		ID:           "openrouter",
		Name:         "OpenRouter",
		Driver:       llm.DriverOpenAICompatible,
		BaseURL:      normalizeProviderBaseURL(base),
		APIKey:       strings.TrimSpace(key),
		DefaultModel: cfg.Defaults.Model,
		ExtraHeaders: map[string]string{
			"HTTP-Referer": "https://github.com/forge-harness/forge",
			"X-Title":      "Forge",
		},
		IsActive: true,
	}
	inserted, err := s.store.SeedDefaultProvider(profile)
	if err != nil {
		logx.Warn("could not seed the default provider", "error", err)
		return
	}
	if inserted {
		logx.Info("seeded default LLM provider", "id", profile.ID, "base_url", profile.BaseURL)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// activeProvider returns the global provider. If the table has rows but none is
// marked active, the oldest row is used. If the table is empty (for example a
// failed seed), legacy OpenRouter config is exposed as a synthetic profile.
func (s *Server) activeProvider() (store.ProviderProfile, error) {
	p, err := s.store.ActiveProvider()
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.ProviderProfile{}, err
	}
	list, err := s.store.ListProviders()
	if err != nil {
		return store.ProviderProfile{}, err
	}
	if len(list) > 0 {
		return list[0], nil
	}
	return s.legacyProvider()
}

func (s *Server) legacyProvider() (store.ProviderProfile, error) {
	cfg := s.cfg.Get()
	base := normalizeProviderBaseURL(cfg.Providers.OpenRouter.BaseURL)
	if base == "" {
		return store.ProviderProfile{}, errors.New("no LLM provider is configured")
	}
	return store.ProviderProfile{
		ID:           "openrouter",
		Name:         "OpenRouter",
		Driver:       llm.DriverOpenAICompatible,
		BaseURL:      base,
		APIKey:       cfg.Providers.OpenRouter.APIKey,
		DefaultModel: cfg.Defaults.Model,
		ExtraHeaders: map[string]string{
			"HTTP-Referer": "https://github.com/forge-harness/forge",
			"X-Title":      "Forge",
		},
		IsActive: true,
	}, nil
}

func (s *Server) providerForSession(session store.Session) (store.ProviderProfile, error) {
	if id := strings.TrimSpace(session.ProviderID); id != "" {
		p, err := s.store.GetProvider(id)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.ProviderProfile{}, err
		}
		logx.Warn("session provider no longer exists, using the active provider",
			"session", session.ID, "provider", id)
	}
	return s.activeProvider()
}

func (s *Server) providerClient(p store.ProviderProfile) (llm.Provider, error) {
	driver := strings.ToLower(strings.TrimSpace(p.Driver))
	if driver == "" {
		driver = llm.DriverOpenAICompatible
	}
	return llm.NewProvider(llm.Profile{
		ID:           p.ID,
		Name:         p.Name,
		Driver:       driver,
		BaseURL:      p.BaseURL,
		APIKey:       p.APIKey,
		DefaultModel: p.DefaultModel,
		ExtraHeaders: p.ExtraHeaders,
	})
}

func (s *Server) defaultModelFor(p store.ProviderProfile) string {
	if model := strings.TrimSpace(p.DefaultModel); model != "" {
		return model
	}
	return s.cfg.Get().Defaults.Model
}

// ---------------------------------------------------------------------------
// handlers

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListProviders()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activeID := ""
	if p, err := s.activeProvider(); err == nil {
		activeID = p.ID
	}
	out := make([]providerDTO, 0, len(list))
	for _, p := range list {
		dto := providerDTOFrom(p)
		if activeID != "" {
			dto.IsActive = dto.ID == activeID
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": out,
		"active_id": activeID,
	})
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerBody
	if err := decodeJSON(w, r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := providerFromBody(in, store.ProviderProfile{}, true)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.ID == "" {
		p.ID = s.uniqueProviderID(p.Name)
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if err := validateProviderProfile(p); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.store.CreateProvider(p)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			status = http.StatusConflict
		}
		apiError(w, status, err.Error())
		return
	}
	s.invalidateModels()
	logx.Info("LLM provider created", "provider", created.ID, "base_url", created.BaseURL, "active", created.IsActive)
	writeJSON(w, http.StatusCreated, map[string]any{"provider": providerDTOFrom(created)})
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	current, err := s.store.GetProvider(id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var in providerBody
	if err := decodeJSON(w, r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := providerFromBody(in, current, false)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.ID = id
	if err := validateProviderProfile(p); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateProvider(p)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.invalidateModels()
	logx.Info("LLM provider updated", "provider", updated.ID, "active", updated.IsActive)
	writeJSON(w, http.StatusOK, map[string]any{"provider": providerDTOFrom(updated)})
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.store.DeleteProvider(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "provider not found")
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.invalidateModels()
	logx.Info("LLM provider deleted", "provider", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var in providerBody
	if err := decodeJSON(w, r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	profile, err := s.testProviderProfile(in)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	client, err := s.providerClient(profile)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTestTimeout)
	defer cancel()
	models, supported, err := client.Probe(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               false,
			"models_supported": supported,
			"message":          err.Error(),
		})
		return
	}
	const maxModels = 60
	trimmed := models
	if len(trimmed) > maxModels {
		trimmed = trimmed[:maxModels]
	}
	message := "Connected. The model catalog is available."
	if !supported {
		message = "Connected. This endpoint does not expose /models; model discovery is unavailable."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"models_supported": supported,
		"count":            len(models),
		"models":           trimmed,
		"message":          message,
	})
}

func (s *Server) testProviderProfile(in providerBody) (store.ProviderProfile, error) {
	base := store.ProviderProfile{}
	if id := strings.TrimSpace(in.ID); id != "" {
		current, err := s.store.GetProvider(id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.ProviderProfile{}, err
		}
		if err == nil {
			base = current
			// Testing an existing provider only needs its id. Fill the blanks
			// from storage so a thin request cannot wipe the stored profile.
			if strings.TrimSpace(in.Name) == "" {
				in.Name = current.Name
			}
			if strings.TrimSpace(in.Driver) == "" {
				in.Driver = current.Driver
			}
			if strings.TrimSpace(in.BaseURL) == "" {
				in.BaseURL = current.BaseURL
			}
			if strings.TrimSpace(in.DefaultModel) == "" {
				in.DefaultModel = current.DefaultModel
			}
			if in.ExtraHeaders == nil {
				in.ExtraHeaders = current.ExtraHeaders
			}
		}
	}
	p, err := providerFromBody(in, base, base.ID == "")
	if err != nil {
		return store.ProviderProfile{}, err
	}
	if p.ID == "" {
		p.ID = "test"
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Driver == "" {
		p.Driver = llm.DriverOpenAICompatible
	}
	if err := validateProviderProfile(p); err != nil {
		return store.ProviderProfile{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// validation / conversion

func providerFromBody(in providerBody, base store.ProviderProfile, creating bool) (store.ProviderProfile, error) {
	p := base
	if creating {
		p = store.ProviderProfile{
			ID:           strings.TrimSpace(in.ID),
			ExtraHeaders: map[string]string{},
			IsActive:     false,
		}
	}
	p.Name = strings.TrimSpace(in.Name)
	p.Driver = strings.TrimSpace(in.Driver)
	p.BaseURL = normalizeProviderBaseURL(in.BaseURL)
	p.DefaultModel = strings.TrimSpace(in.DefaultModel)
	if p.Driver == "" {
		p.Driver = llm.DriverOpenAICompatible
	}
	if in.ExtraHeaders != nil {
		p.ExtraHeaders = make(map[string]string, len(in.ExtraHeaders))
		for k, v := range in.ExtraHeaders {
			p.ExtraHeaders[k] = v
		}
	}
	if in.IsActive != nil {
		p.IsActive = *in.IsActive
	}
	// An empty key or the keep sentinel preserves the stored secret. This is
	// what lets the UI show "configured — leave blank to keep".
	if in.APIKey != "" && in.APIKey != keepAPIKey && !strings.Contains(in.APIKey, "••") {
		p.APIKey = strings.TrimSpace(in.APIKey)
	}
	return p, nil
}

func validateProviderProfile(p store.ProviderProfile) error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("provider id is required")
	}
	if !validProviderID(p.ID) {
		return fmt.Errorf("provider id must use lowercase letters, digits, '-' or '_'")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("provider name is required")
	}
	driver := strings.ToLower(strings.TrimSpace(p.Driver))
	if driver != llm.DriverOpenAICompatible {
		return fmt.Errorf("unsupported provider driver %q", p.Driver)
	}
	if _, err := parseProviderBaseURL(p.BaseURL); err != nil {
		return err
	}
	for name := range p.ExtraHeaders {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("extra header names cannot be empty")
		}
	}
	return nil
}

func parseProviderBaseURL(raw string) (string, error) {
	raw = normalizeProviderBaseURL(raw)
	if raw == "" {
		return "", fmt.Errorf("provider base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("provider base URL must start with http:// or https://")
	}
	return raw, nil
}

func normalizeProviderBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suffix := range []string{"/chat/completions", "/models"} {
		if strings.HasSuffix(raw, suffix) {
			raw = strings.TrimSuffix(raw, suffix)
			raw = strings.TrimRight(raw, "/")
			break
		}
	}
	return raw
}

func validProviderID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_'):
		default:
			return false
		}
	}
	return true
}

func (s *Server) uniqueProviderID(name string) string {
	base := slugifyProviderID(name)
	if base == "" {
		base = "provider"
	}
	if _, err := s.store.GetProvider(base); errors.Is(err, store.ErrNotFound) {
		return base
	}
	for i := 2; i < 1000; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if _, err := s.store.GetProvider(id); errors.Is(err, store.ErrNotFound) {
			return id
		}
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func slugifyProviderID(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	return out
}

func providerDTOFrom(p store.ProviderProfile) providerDTO {
	headers := make(map[string]string, len(p.ExtraHeaders))
	for k, v := range p.ExtraHeaders {
		headers[k] = v
	}
	return providerDTO{
		ID:           p.ID,
		Name:         p.Name,
		Driver:       p.Driver,
		BaseURL:      p.BaseURL,
		DefaultModel: p.DefaultModel,
		ExtraHeaders: headers,
		IsActive:     p.IsActive,
		APIKeySet:    p.APIKey != "",
		APIKeyHint:   maskAPIKey(p.APIKey),
		CreatedAt:    p.CreatedAt,
	}
}
