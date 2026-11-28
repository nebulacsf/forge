package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectFilePaths(t *testing.T, root string, limit int) map[string]ProjectFile {
	t.Helper()
	files, err := ProjectFiles(root, limit)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]ProjectFile, len(files))
	for _, f := range files {
		got[f.Path] = f
	}
	return got
}

func TestProjectFilesWithoutGitShowsDotfiles(t *testing.T) {
	m, root := newTestManager(t, "BUILD", 4096)
	m.Mode = "BUILD"
	if LoadFileSet(context.Background(), root).Loaded() {
		t.Skip("the temporary directory lives inside a repository")
	}

	got := projectFilePaths(t, root, 0)
	for _, want := range []string{"hello.go", "sub/nested.txt", ".hidden"} {
		if _, ok := got[want]; !ok {
			t.Errorf("ProjectFiles missing %q (got %v)", want, got)
		}
	}
	if lang := got["hello.go"].Language; lang != "go" {
		t.Errorf("hello.go language = %q, want go", lang)
	}
	if got["hello.go"].SizeBytes <= 0 {
		t.Errorf("hello.go size = %d, want > 0", got["hello.go"].SizeBytes)
	}

	// The git directory is never offered as a project file.
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path := range projectFilePaths(t, root, 0) {
		if strings.HasPrefix(path, ".git/") {
			t.Fatalf("noise directory not pruned: %s", path)
		}
	}

	// The limit is honoured.
	limited, err := ProjectFiles(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("limit 1 returned %d files", len(limited))
	}
}

func TestProjectFilesFollowGitIgnore(t *testing.T) {
	root := fileSetRepo(t)

	got := projectFilePaths(t, root, 0)
	for _, want := range []string{".github/workflows/ci.yml", "main.go"} {
		if _, ok := got[want]; !ok {
			t.Errorf("ProjectFiles missing tracked %q (got %v)", want, got)
		}
	}
	for _, notWant := range []string{".env", "vendor/dep/dep.go"} {
		if _, ok := got[notWant]; ok {
			t.Errorf("ProjectFiles must not list %q", notWant)
		}
	}
	if lang := got[".github/workflows/ci.yml"].Language; lang != "" {
		t.Errorf("yml should have no indexed language, got %q", lang)
	}
}

func TestProjectFilesRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectFiles(file, 0); err == nil {
		t.Fatal("expected an error for a file root")
	}
}

func TestReadAttachment(t *testing.T) {
	_, root := newTestManager(t, "BUILD", 4096)

	content, err := ReadAttachment(root, "hello.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "func Greeting") {
		t.Errorf("content = %q", content)
	}

	truncated, err := ReadAttachment(root, "hello.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(truncated, "attachment truncated") {
		t.Errorf("expected truncation marker, got %q", truncated)
	}

	if _, err := ReadAttachment(root, "blob.bin", 0); err == nil {
		t.Error("binary files must be rejected")
	}
	if _, err := ReadAttachment(root, "../escape.txt", 0); err == nil {
		t.Error("path traversal must be rejected")
	}
	if _, err := ReadAttachment(root, "sub", 0); err == nil {
		t.Error("directories must be rejected")
	}
}
