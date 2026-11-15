package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectFilesSkipsHiddenAndBinary(t *testing.T) {
	m, root := newTestManager(t, "BUILD", 4096)
	m.Mode = "BUILD"

	files, err := ProjectFiles(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProjectFile{}
	for _, f := range files {
		got[f.Path] = f
	}
	for _, want := range []string{"hello.go", "sub/nested.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("ProjectFiles missing %q (got %v)", want, files)
		}
	}
	if _, ok := got[".hidden"]; ok {
		t.Error("ProjectFiles must skip hidden files")
	}
	if lang := got["hello.go"].Language; lang != "go" {
		t.Errorf("hello.go language = %q, want go", lang)
	}
	if got["hello.go"].SizeBytes <= 0 {
		t.Errorf("hello.go size = %d, want > 0", got["hello.go"].SizeBytes)
	}

	// Hidden directories are pruned as well.
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err = ProjectFiles(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, ".git/") {
			t.Fatalf("hidden directory not pruned: %s", f.Path)
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
