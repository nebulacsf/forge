package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ProviderProfile is one configured LLM endpoint. APIKey is the raw secret and
// is never returned to the browser unmodified by the server layer.
type ProviderProfile struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Driver       string            `json:"driver"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key,omitempty"`
	DefaultModel string            `json:"default_model"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	IsActive     bool              `json:"is_active"`
	CreatedAt    string            `json:"created_at"`
}

func (s *Store) CountProviders() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM llm_providers`).Scan(&n)
	return n, err
}

// ListProviders returns every configured provider, active first.
func (s *Store) ListProviders() ([]ProviderProfile, error) {
	rows, err := s.db.Query(`
		SELECT id, name, driver, base_url, api_key, default_model,
		       extra_headers_json, is_active, created_at
		FROM llm_providers
		ORDER BY is_active DESC, name COLLATE NOCASE ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderProfile{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider loads one profile by id.
func (s *Store) GetProvider(id string) (ProviderProfile, error) {
	row := s.db.QueryRow(`
		SELECT id, name, driver, base_url, api_key, default_model,
		       extra_headers_json, is_active, created_at
		FROM llm_providers WHERE id = ?`, strings.TrimSpace(id))
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderProfile{}, ErrNotFound
	}
	return p, err
}

// ActiveProvider returns the globally active profile.
func (s *Store) ActiveProvider() (ProviderProfile, error) {
	row := s.db.QueryRow(`
		SELECT id, name, driver, base_url, api_key, default_model,
		       extra_headers_json, is_active, created_at
		FROM llm_providers WHERE is_active = 1
		ORDER BY created_at ASC LIMIT 1`)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderProfile{}, ErrNotFound
	}
	return p, err
}

// CreateProvider inserts a profile. The first profile and any profile created
// with IsActive become the single active row.
func (s *Store) CreateProvider(p ProviderProfile) (ProviderProfile, error) {
	p = normalizeProvider(p)
	if p.ID == "" {
		return ProviderProfile{}, fmt.Errorf("provider id is required")
	}
	if p.BaseURL == "" {
		return ProviderProfile{}, fmt.Errorf("provider base URL is required")
	}
	headers, err := encodeHeaders(p.ExtraHeaders)
	if err != nil {
		return ProviderProfile{}, err
	}
	p.CreatedAt = now()

	tx, err := s.db.Begin()
	if err != nil {
		return ProviderProfile{}, err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM llm_providers`).Scan(&count); err != nil {
		return ProviderProfile{}, err
	}
	if p.IsActive || count == 0 {
		if _, err := tx.Exec(`UPDATE llm_providers SET is_active = 0 WHERE is_active = 1`); err != nil {
			return ProviderProfile{}, err
		}
		p.IsActive = true
	}
	if _, err := tx.Exec(`
		INSERT INTO llm_providers
			(id, name, driver, base_url, api_key, default_model, extra_headers_json, is_active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Driver, p.BaseURL, p.APIKey, p.DefaultModel, headers, boolInt(p.IsActive), p.CreatedAt,
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ProviderProfile{}, fmt.Errorf("a provider with id %q already exists", p.ID)
		}
		return ProviderProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderProfile{}, err
	}
	return s.GetProvider(p.ID)
}

// UpdateProvider replaces the editable fields of an existing profile. The
// caller merges a preserved API key before calling this method.
func (s *Store) UpdateProvider(p ProviderProfile) (ProviderProfile, error) {
	p = normalizeProvider(p)
	if p.BaseURL == "" {
		return ProviderProfile{}, fmt.Errorf("provider base URL is required")
	}
	headers, err := encodeHeaders(p.ExtraHeaders)
	if err != nil {
		return ProviderProfile{}, err
	}
	current, err := s.GetProvider(p.ID)
	if err != nil {
		return ProviderProfile{}, err
	}
	p.CreatedAt = current.CreatedAt

	tx, err := s.db.Begin()
	if err != nil {
		return ProviderProfile{}, err
	}
	defer tx.Rollback()
	if p.IsActive {
		if _, err := tx.Exec(`UPDATE llm_providers SET is_active = 0 WHERE is_active = 1 AND id <> ?`, p.ID); err != nil {
			return ProviderProfile{}, err
		}
	}
	res, err := tx.Exec(`
		UPDATE llm_providers
		SET name = ?, driver = ?, base_url = ?, api_key = ?, default_model = ?,
		    extra_headers_json = ?, is_active = ?
		WHERE id = ?`,
		p.Name, p.Driver, p.BaseURL, p.APIKey, p.DefaultModel, headers, boolInt(p.IsActive), p.ID)
	if err != nil {
		return ProviderProfile{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ProviderProfile{}, ErrNotFound
	}
	// Keep at least one active provider. Clearing the active checkbox on the
	// last active profile is a no-op rather than leaving the app without a
	// default endpoint.
	if !p.IsActive {
		var activeCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM llm_providers WHERE is_active = 1`).Scan(&activeCount); err != nil {
			return ProviderProfile{}, err
		}
		if activeCount == 0 {
			if _, err := tx.Exec(`UPDATE llm_providers SET is_active = 1 WHERE id = ?`, p.ID); err != nil {
				return ProviderProfile{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ProviderProfile{}, err
	}
	return s.GetProvider(p.ID)
}

// SetActiveProvider makes one profile globally active and clears every other.
func (s *Store) SetActiveProvider(id string) (ProviderProfile, error) {
	id = strings.TrimSpace(id)
	tx, err := s.db.Begin()
	if err != nil {
		return ProviderProfile{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM llm_providers WHERE id = ?`, id).Scan(&exists); err != nil {
		return ProviderProfile{}, err
	}
	if exists == 0 {
		return ProviderProfile{}, ErrNotFound
	}
	// Clear first: the partial unique index allows only one active row, so
	// setting the new row before clearing the old one would violate it.
	if _, err := tx.Exec(`UPDATE llm_providers SET is_active = 0 WHERE is_active = 1 AND id <> ?`, id); err != nil {
		return ProviderProfile{}, err
	}
	res, err := tx.Exec(`UPDATE llm_providers SET is_active = 1 WHERE id = ?`, id)
	if err != nil {
		return ProviderProfile{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ProviderProfile{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return ProviderProfile{}, err
	}
	return s.GetProvider(id)
}

// DeleteProvider removes a profile. Sessions pinned to it fall back to the
// active provider, and deleting the active profile promotes another one.
func (s *Store) DeleteProvider(id string) error {
	id = strings.TrimSpace(id)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var wasActive int
	if err := tx.QueryRow(`SELECT is_active FROM llm_providers WHERE id = ?`, id).Scan(&wasActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(`DELETE FROM llm_providers WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sessions SET provider_id = '' WHERE provider_id = ?`, id); err != nil {
		return err
	}
	if wasActive != 0 {
		if _, err := tx.Exec(`
			UPDATE llm_providers SET is_active = 1
			WHERE id = (SELECT id FROM llm_providers ORDER BY created_at ASC LIMIT 1)`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedDefaultProvider inserts a profile only when no provider exists. It is
// idempotent and used to migrate legacy OpenRouter config on first launch.
func (s *Store) SeedDefaultProvider(p ProviderProfile) (bool, error) {
	n, err := s.CountProviders()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	p.IsActive = true
	if _, err := s.CreateProvider(p); err != nil {
		return false, err
	}
	return true, nil
}

func scanProvider(r rowScanner) (ProviderProfile, error) {
	var (
		p       ProviderProfile
		active  int
		headers string
	)
	if err := r.Scan(&p.ID, &p.Name, &p.Driver, &p.BaseURL, &p.APIKey,
		&p.DefaultModel, &headers, &active, &p.CreatedAt); err != nil {
		return ProviderProfile{}, err
	}
	p.IsActive = active != 0
	p.ExtraHeaders = map[string]string{}
	if strings.TrimSpace(headers) != "" {
		if err := json.Unmarshal([]byte(headers), &p.ExtraHeaders); err != nil {
			return ProviderProfile{}, fmt.Errorf("decode extra headers for provider %s: %w", p.ID, err)
		}
	}
	return p, nil
}

func normalizeProvider(p ProviderProfile) ProviderProfile {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = p.ID
	}
	p.Driver = strings.ToLower(strings.TrimSpace(p.Driver))
	if p.Driver == "" {
		p.Driver = "openai_compatible"
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.APIKey = strings.TrimSpace(p.APIKey)
	p.DefaultModel = strings.TrimSpace(p.DefaultModel)
	if p.ExtraHeaders == nil {
		p.ExtraHeaders = map[string]string{}
	}
	return p
}

func encodeHeaders(headers map[string]string) (string, error) {
	if headers == nil {
		return "{}", nil
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = value
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode extra headers: %w", err)
	}
	return string(data), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
