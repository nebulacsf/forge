package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestProviderCRUDAndActiveInvariant(t *testing.T) {
	st := openTestStore(t)

	first, err := st.CreateProvider(ProviderProfile{
		ID: "first", Name: "First", BaseURL: "http://localhost:1111/v1",
		ExtraHeaders: map[string]string{"X-Title": "Forge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsActive {
		t.Error("the first provider should become active")
	}

	second, err := st.CreateProvider(ProviderProfile{
		ID: "second", Name: "Second", BaseURL: "http://localhost:2222/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.IsActive {
		t.Error("a second provider should not be active by default")
	}

	list, err := st.ListProviders()
	if err != nil || len(list) != 2 {
		t.Fatalf("ListProviders = %+v, %v", list, err)
	}

	active, err := st.ActiveProvider()
	if err != nil || active.ID != "first" {
		t.Fatalf("ActiveProvider = %+v, %v", active, err)
	}

	if _, err := st.SetActiveProvider("second"); err != nil {
		t.Fatal(err)
	}
	active, err = st.ActiveProvider()
	if err != nil || active.ID != "second" {
		t.Fatalf("after switch, ActiveProvider = %+v, %v", active, err)
	}

	first.IsActive = true
	first.DefaultModel = "model-a"
	if _, err := st.UpdateProvider(first); err != nil {
		t.Fatal(err)
	}
	active, _ = st.ActiveProvider()
	if active.ID != "first" || active.DefaultModel != "model-a" {
		t.Fatalf("updated active provider = %+v", active)
	}

	// Clearing the only active flag is a no-op: Forge always has a default.
	first.IsActive = false
	if _, err := st.UpdateProvider(first); err != nil {
		t.Fatal(err)
	}
	active, _ = st.ActiveProvider()
	if active.ID != "first" {
		t.Fatalf("active provider after clearing = %+v", active)
	}

	if err := st.DeleteProvider("first"); err != nil {
		t.Fatal(err)
	}
	active, err = st.ActiveProvider()
	if err != nil || active.ID != "second" {
		t.Fatalf("delete active should promote the next provider: %+v, %v", active, err)
	}

	if err := st.DeleteProvider("second"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountProviders(); err != nil || n != 0 {
		t.Fatalf("providers after delete = %d, %v", n, err)
	}
}

func TestSeedDefaultProviderIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	p := ProviderProfile{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1"}

	inserted, err := st.SeedDefaultProvider(p)
	if err != nil || !inserted {
		t.Fatalf("first seed = %v, %v", inserted, err)
	}
	inserted, err = st.SeedDefaultProvider(ProviderProfile{
		ID: "other", Name: "Other", BaseURL: "http://localhost:1234/v1",
	})
	if err != nil || inserted {
		t.Fatalf("second seed = %v, %v", inserted, err)
	}
	if n, _ := st.CountProviders(); n != 1 {
		t.Fatalf("provider count = %d, want 1", n)
	}
}

func TestSessionProviderBinding(t *testing.T) {
	st := openTestStore(t)
	project, err := st.CreateProject("P", "/tmp/provider-binding", "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Chat", "model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	if session.ProviderID != "" {
		t.Fatalf("new session provider = %q, want empty", session.ProviderID)
	}
	if _, err := st.CreateProvider(ProviderProfile{
		ID: "local", Name: "Local", BaseURL: "http://localhost:11434/v1",
	}); err != nil {
		t.Fatal(err)
	}

	pinned, err := st.SetSessionProvider(session.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ProviderID != "local" {
		t.Fatalf("pinned provider = %q", pinned.ProviderID)
	}
	got, err := st.GetSession(session.ID)
	if err != nil || got.ProviderID != "local" {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}

	cleared, err := st.SetSessionProvider(session.ID, "")
	if err != nil || cleared.ProviderID != "" {
		t.Fatalf("cleared provider = %+v, %v", cleared, err)
	}

	if _, err := st.SetSessionProvider(session.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing provider error = %v, want ErrNotFound", err)
	}
}

func TestMigrationAddsProviderTableAndSessionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := `
CREATE TABLE projects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	path TEXT NOT NULL UNIQUE,
	icon_color TEXT NOT NULL DEFAULT '#6c8cff',
	last_opened_at TEXT NOT NULL
);
CREATE TABLE sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL,
	title TEXT NOT NULL DEFAULT 'New Chat',
	model TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'PLAN',
	tokens_in INTEGER NOT NULL DEFAULT 0,
	tokens_out INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	is_open INTEGER NOT NULL DEFAULT 1,
	updated_at TEXT NOT NULL
);
`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	project, err := st.CreateProject("P", "/tmp/old-db", "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Chat", "model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	if session.ProviderID != "" {
		t.Fatalf("provider id after migration = %q", session.ProviderID)
	}
	if _, err := st.CreateProvider(ProviderProfile{
		ID: "local", Name: "Local", BaseURL: "http://localhost:1234/v1",
	}); err != nil {
		t.Fatalf("provider table missing after migration: %v", err)
	}
}
