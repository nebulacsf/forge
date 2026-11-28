package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fileSetRepo builds a repository with a tracked dotfile, an ignored file, an
// untracked file that no rule covers and a tracked file inside a noise
// directory, which is every case the file set has to distinguish.
func fileSetRepo(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)
	writeProjectFile(t, dir, ".github/workflows/ci.yml", "name: ci\n")
	writeProjectFile(t, dir, "main.go", "package main\n")
	writeProjectFile(t, dir, "vendor/dep/dep.go", "package dep\n")
	gitRun(t, dir, "add", ".github/workflows/ci.yml", "main.go", "vendor/dep/dep.go")
	gitRun(t, dir, "commit", "-q", "-m", "track dotfile and noise")

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, dir, ".env", "SECRET=1\n")
	writeProjectFile(t, dir, "notes.txt", "local\n")
	return dir
}

func TestLoadFileSetFollowsGit(t *testing.T) {
	dir := fileSetRepo(t)

	set := LoadFileSet(context.Background(), dir)
	if !set.Loaded() {
		t.Fatal("file set unavailable for a repository")
	}
	if set.Root() != dir {
		t.Errorf("root = %q, want %q", set.Root(), dir)
	}

	// Tracked dotfiles are part of the project; that is the whole point.
	for _, want := range []string{".github/workflows/ci.yml", "main.go", "README.md"} {
		if !set.Has(want) {
			t.Errorf("file set missing tracked %q", want)
		}
	}
	// Untracked but not ignored: still part of the project.
	if !set.Has("notes.txt") {
		t.Error("file set must include untracked files no ignore rule covers")
	}
	if !set.Has(".gitignore") {
		t.Error("file set must include the untracked .gitignore itself")
	}
	// Ignored and internal git state stay out.
	for _, notWant := range []string{".env", ".git/config"} {
		if set.Has(notWant) {
			t.Errorf("file set must not include %q", notWant)
		}
	}
	// A committed vendor/ tree is part of the repository but skipped by design.
	if set.Has("vendor/dep/dep.go") {
		t.Error("noise directories are still excluded")
	}

	if !set.HasDir(".github") || !set.HasDir("") {
		t.Error("directories that hold project files must be reported as project directories")
	}
	if set.HasDir("vendor") {
		t.Error("vendor must not be a project directory")
	}
}

func TestLoadFileSetWithoutRepository(t *testing.T) {
	dir := t.TempDir()
	set := LoadFileSet(context.Background(), dir)
	if set.Loaded() {
		t.Skip("the temporary directory lives inside a repository")
	}
	if set.Len() != 0 || set.Has("main.go") || set.HasDir("") {
		t.Fatalf("a plain directory must yield an empty, non-loaded set: %+v", set.Files())
	}
}
