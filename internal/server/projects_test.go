package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"forge/internal/store"
)

func TestPatchAndDeleteProject(t *testing.T) {
	cfg, st, _ := modelsFixture(t)

	moved := filepath.Join(t.TempDir(), "moved-project")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatal(err)
	}
	origin := t.TempDir()
	project, err := st.CreateProject("Demo", origin, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(project.ID, "Chat", "model", "PLAN"); err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS("."), "test").Handler())
	defer app.Close()

	patch := func(body map[string]any) (*http.Response, map[string]any) {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPatch, app.URL+"/api/projects/"+itoa(project.ID), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		return resp, payload
	}

	// Rename + recolor without touching the path.
	resp, payload := patch(map[string]any{"name": "Renamed", "icon_color": "#22c55e"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d payload = %v", resp.StatusCode, payload)
	}
	updated, _ := st.GetProject(project.ID)
	if updated.Name != "Renamed" || updated.IconColor != "#22c55e" || updated.Path != origin {
		t.Fatalf("project = %+v", updated)
	}

	// Moving the directory keeps the project identity and its chats.
	resp, payload = patch(map[string]any{"path": moved})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d payload = %v", resp.StatusCode, payload)
	}
	updated, _ = st.GetProject(project.ID)
	if updated.Path != moved {
		t.Fatalf("path = %q, want %q", updated.Path, moved)
	}
	sessions, err := st.ListSessions(project.ID, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions after move = %+v, %v", sessions, err)
	}

	// Invalid color and a missing directory are rejected.
	if resp, _ := patch(map[string]any{"icon_color": "blue"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad color status = %d", resp.StatusCode)
	}
	if resp, _ := patch(map[string]any{"path": filepath.Join(moved, "does-not-exist")}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path status = %d", resp.StatusCode)
	}

	// Empty name falls back to the directory base name.
	if resp, _ := patch(map[string]any{"name": "   "}); resp.StatusCode != http.StatusOK {
		t.Fatalf("blank name status = %d", resp.StatusCode)
	}
	if updated, _ = st.GetProject(project.ID); updated.Name != "moved-project" {
		t.Fatalf("name = %q, want moved-project", updated.Name)
	}

	req, _ := http.NewRequest(http.MethodDelete, app.URL+"/api/projects/"+itoa(project.ID), nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", del.StatusCode)
	}
	if _, err := st.GetProject(project.ID); err == nil {
		t.Fatal("project should be gone")
	}
	if sessions, _ := st.ListSessions(project.ID, false); len(sessions) != 0 {
		t.Fatalf("sessions should cascade, got %+v", sessions)
	}
}

// TestProjectGitEndpoint covers the workspace bar contract: a missing project is
// a 404 and a plain directory answers is_repo=false instead of an error, because
// "not a repository" is a normal state and not something the user can fix.
func TestProjectGitEndpoint(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	project, err := st.CreateProject("Plain", t.TempDir(), "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS("."), "test").Handler())
	defer app.Close()

	get := func(id string) (*http.Response, gitPayload) {
		t.Helper()
		resp, err := http.Get(app.URL + "/api/projects/" + id + "/git")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload gitPayload
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		return resp, payload
	}

	resp, payload := get(itoa(project.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d payload = %+v", resp.StatusCode, payload)
	}
	if payload.ProjectID != project.ID || payload.Root != project.Path {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.IsRepo || payload.Branch != "" {
		t.Fatalf("a bare temp dir must not report a branch: %+v", payload)
	}

	if resp, _ := get("999999"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing project status = %d", resp.StatusCode)
	}
	if resp, _ := get("abc"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id status = %d", resp.StatusCode)
	}
}

type gitPayload struct {
	ProjectID int64  `json:"project_id"`
	Root      string `json:"root"`
	IsRepo    bool   `json:"is_repo"`
	Branch    string `json:"branch"`
	Detached  bool   `json:"detached"`
	Commit    string `json:"commit"`
}

func itoa(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

var _ = store.ErrNotFound
