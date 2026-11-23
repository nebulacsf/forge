package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/store"
	"forge/internal/tools"
)

// TestBuildInstructionBlockRendersSources pins the shape of the injected block:
// one labeled section per source, the harness precedence note, and nothing at all
// when the project ships no artifact.
func TestBuildInstructionBlockRendersSources(t *testing.T) {
	if got := buildInstructionBlock(nil); got != "" {
		t.Errorf("empty project must inject nothing, got %q", got)
	}

	block := buildInstructionBlock([]tools.InstructionFile{
		{Path: "AGENTS.md", Kind: "agent", Content: "Run gofmt before commits.", Chars: 27},
		{Path: "README.md", Kind: "readme", Content: "# Demo", Chars: 6, Truncated: true},
	})
	for _, want := range []string{
		"captured automatically from this repository",
		"not a new request",
		"AGENTS.md (27 chars)",
		"Run gofmt before commits.",
		"Background documentation (no agent instruction file was found)",
		"README.md (6 chars, truncated)",
		"take precedence on any conflict",
		"apply to new chats only",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("instructions block missing %q:\n%s", want, block)
		}
	}
}

// TestSessionInstructionsCapturedOnce is the core contract of this feature: the
// snapshot is read from disk exactly once per chat and reused verbatim, so the
// system prompt stays stable across turns even if the file changes on disk.
func TestSessionInstructionsCapturedOnce(t *testing.T) {
	_, st, _ := modelsFixture(t)
	projectDir := t.TempDir()
	writeProjectFile(t, projectDir, "AGENTS.md", "Always run the tests.")

	project, err := st.CreateProject("Snapshot", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Chat", "mock/model", "BUILD")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st}

	first := srv.sessionInstructions(session, project)
	if !strings.Contains(first, "Always run the tests.") {
		t.Fatalf("first snapshot missing the repo instructions:\n%s", first)
	}
	stored, captured, err := st.GetSessionInstructions(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !captured || stored != first {
		t.Fatalf("snapshot not persisted (captured=%v):\n%s", captured, stored)
	}

	// Editing the file must not change what this chat already captured.
	writeProjectFile(t, projectDir, "AGENTS.md", "Completely different rules.")
	second := srv.sessionInstructions(session, project)
	if second != first {
		t.Errorf("snapshot changed mid-session:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(second, "Completely different rules.") {
		t.Error("session picked up an on-disk edit instead of using its snapshot")
	}
}

// TestSessionInstructionsEmptyProjectIsReadOnce keeps a project without artifacts
// from re-stat'ing every recognized path on each turn of every chat.
func TestSessionInstructionsEmptyProjectIsReadOnce(t *testing.T) {
	_, st, _ := modelsFixture(t)
	projectDir := t.TempDir()
	writeProjectFile(t, projectDir, "main.go", "package main\n")

	project, err := st.CreateProject("Bare", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Chat", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st}

	if got := srv.sessionInstructions(session, project); got != "" {
		t.Fatalf("project without artifacts injected %q", got)
	}
	if _, captured, err := st.GetSessionInstructions(session.ID); err != nil || !captured {
		t.Fatalf("empty snapshot must still be marked captured (captured=%v, err=%v)", captured, err)
	}
	// A file appearing later is ignored: the chat keeps its captured snapshot.
	writeProjectFile(t, projectDir, "AGENTS.md", "late rules")
	if got := srv.sessionInstructions(session, project); got != "" {
		t.Errorf("late file leaked into an existing chat: %q", got)
	}
}

func writeProjectFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStoreSessionInstructionsDistinguishesUncaptured covers the NULL versus
// empty-string contract the lazy capture relies on.
func TestStoreSessionInstructionsDistinguishesUncaptured(t *testing.T) {
	_, st, _ := modelsFixture(t)
	project, err := st.CreateProject("Instructions", t.TempDir(), "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Chat", "mock/model", "BUILD")
	if err != nil {
		t.Fatal(err)
	}
	if text, captured, err := st.GetSessionInstructions(session.ID); err != nil || captured || text != "" {
		t.Fatalf("fresh session = (%q, %v, %v), want uncaptured", text, captured, err)
	}
	if err := st.SetSessionInstructions(session.ID, ""); err != nil {
		t.Fatal(err)
	}
	if text, captured, err := st.GetSessionInstructions(session.ID); err != nil || !captured || text != "" {
		t.Fatalf("after empty capture = (%q, %v, %v), want captured and empty", text, captured, err)
	}
	if err := st.SetSessionInstructions(999999, "x"); err != store.ErrNotFound {
		t.Fatalf("unknown session error = %v, want ErrNotFound", err)
	}
}
