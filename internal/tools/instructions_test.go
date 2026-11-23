package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstructionFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func instructionPaths(files []InstructionFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestReadProjectInstructionsPriority pins the discovery order: AGENTS.md wins
// over the other agent artifacts and README.md is only a fallback.
func TestReadProjectInstructionsPriority(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, root, "README.md", "# Demo\n")
	writeInstructionFile(t, root, "CLAUDE.md", "claude rules")
	writeInstructionFile(t, root, ".cursorrules", "cursor rules")
	writeInstructionFile(t, root, ".github/copilot-instructions.md", "copilot rules")

	files, err := ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if message, ok := hasDuplicateInstruction(files); ok {
		t.Fatalf("duplicate instruction content injected twice: %q", message)
	}
	got := instructionPaths(files)
	want := []string{"CLAUDE.md", ".cursorrules", ".github/copilot-instructions.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for _, f := range files {
		if f.Kind != "agent" {
			t.Errorf("%s kind = %q, want agent", f.Path, f.Kind)
		}
	}

	writeInstructionFile(t, root, "AGENTS.md", "agents rules")
	files, err = ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if paths := instructionPaths(files); len(paths) == 0 || paths[0] != "AGENTS.md" {
		t.Fatalf("AGENTS.md must be first, got %v", paths)
	}
}

// TestReadProjectInstructionsReadmeFallback checks README.md is injected with
// its own kind only when the repository ships no agent instruction file.
func TestReadProjectInstructionsReadmeFallback(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, root, "README.md", "# Demo\n\nHow to build.\n")

	files, err := ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, want just the README", instructionPaths(files))
	}
	if files[0].Path != "README.md" || files[0].Kind != "readme" {
		t.Fatalf("file = %+v, want README.md of kind readme", files[0])
	}
}

// TestReadProjectInstructionsEmpty covers a project without artifacts: the
// loader must not fail, so the session snapshot can be stored as "nothing".
func TestReadProjectInstructionsEmpty(t *testing.T) {
	files, err := ReadProjectInstructions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none", instructionPaths(files))
	}
}

// TestReadProjectInstructionsDedupesSymlink covers the common convention of
// symlinking CLAUDE.md to AGENTS.md: the text must not be injected twice.
func TestReadProjectInstructionsDedupesSymlink(t *testing.T) {
	root := t.TempDir()
	writeInstructionFile(t, root, "AGENTS.md", "single source of truth")
	if err := os.Symlink(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, err := ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "AGENTS.md" {
		t.Fatalf("files = %v, want only AGENTS.md", instructionPaths(files))
	}
}

// TestReadProjectInstructionsBudgetTruncates keeps an oversized AGENTS.md from
// swallowing the context window, and flags the truncation.
func TestReadProjectInstructionsBudgetTruncates(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("convention line\n", instructionMaxAgentChars)
	writeInstructionFile(t, root, "AGENTS.md", big)

	files, err := ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, want one entry", instructionPaths(files))
	}
	if !files[0].Truncated {
		t.Error("oversized AGENTS.md was not flagged as truncated")
	}
	if files[0].Chars > instructionMaxAgentChars+len(instructionTruncatedMarker) {
		t.Errorf("chars = %d, budget is %d", files[0].Chars, instructionMaxAgentChars)
	}
}

// TestReadProjectInstructionsSkipsBinary keeps a binary AGENTS.md out of the
// prompt instead of injecting control characters.
func TestReadProjectInstructionsSkipsBinary(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(abs, []byte("rules\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ReadProjectInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none for a binary artifact", instructionPaths(files))
	}
}

func hasDuplicateInstruction(files []InstructionFile) (string, bool) {
	seen := map[string]bool{}
	for _, f := range files {
		key := strings.Join(strings.Fields(f.Content), " ")
		if seen[key] {
			return key, true
		}
		seen[key] = true
	}
	return "", false
}
