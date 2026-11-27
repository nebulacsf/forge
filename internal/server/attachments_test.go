package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/store"
)

func attachmentFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "util.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildAttachmentBlockInlinesFiles(t *testing.T) {
	root := attachmentFixture(t)

	if block := buildAttachmentBlock(root, nil); block != "" {
		t.Errorf("empty input should produce no block, got %q", block)
	}

	block := buildAttachmentBlock(root, []string{"main.go", "main.go", "../escape.txt"})
	for _, want := range []string{
		attachmentOpenTag,
		attachmentCloseTag,
		`<file path="main.go"`,
		"func main()",
		`error=`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	if n := strings.Count(block, `<file path="main.go"`); n != 1 {
		t.Errorf("duplicate paths must be collapsed, got %d entries", n)
	}
}

func TestBuildAttachmentBlockCapsFilesAndBudget(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 0, attachmentMaxFiles+2)
	for i := 0; i < attachmentMaxFiles+2; i++ {
		name := "f" + itoa(int64(i)) + ".txt"
		if err := os.WriteFile(filepath.Join(root, name), []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	block := buildAttachmentBlock(root, paths)
	if n := strings.Count(block, "<file path="); n != attachmentMaxFiles {
		t.Errorf("attached %d files, want %d", n, attachmentMaxFiles)
	}
}

func TestStripAttachmentBlocksAndPathList(t *testing.T) {
	content := "explain this\n\n" +
		attachmentOpenTag + "\n" +
		`<file path="main.go" chars="20">` + "\npackage main\n" + "</file>\n" +
		attachmentCloseTag

	if got := stripAttachmentBlocks(content); got != "explain this" {
		t.Errorf("stripAttachmentBlocks = %q, want %q", got, "explain this")
	}
	if got := stripAttachmentBlocks("plain text"); got != "plain text" {
		t.Errorf("plain text should pass through, got %q", got)
	}
	if got := stripAttachmentBlocks("only head\n" + attachmentOpenTag + "\n<file path=\"a\">"); got != "only head" {
		t.Errorf("unterminated block should be trimmed, got %q", got)
	}

	paths := attachmentPathList(content)
	if !strings.Contains(paths, "[attachment] main.go") {
		t.Errorf("attachmentPathList = %q", paths)
	}
	if strings.Contains(paths, "package main") {
		t.Errorf("attachmentPathList must not include file content: %q", paths)
	}
	if paths := attachmentPathList("no attachments"); paths != "" {
		t.Errorf("attachmentPathList without a block = %q, want empty", paths)
	}
}

func TestCompactMessageTextDropsAttachmentContent(t *testing.T) {
	body := "why does this fail?\n\n" + attachmentOpenTag + "\n" +
		`<file path="main.go" chars="5000">` + "\n" + strings.Repeat("x", 5000) + "\n</file>\n" +
		attachmentCloseTag

	text := compactMessageText(store.Message{Role: "user", Content: body})
	if !strings.Contains(text, "why does this fail?") {
		t.Errorf("summary lost the user intent: %q", text)
	}
	if len(text) > 200 {
		t.Errorf("summary kept attachment content (%d chars)", len(text))
	}

	onlyFiles := attachmentOpenTag + "\n<file path=\"main.go\">\nbody\n</file>\n" + attachmentCloseTag
	text = compactMessageText(store.Message{Role: "user", Content: onlyFiles})
	if !strings.Contains(text, "[attachment] main.go") {
		t.Errorf("summary should keep attachment paths, got %q", text)
	}
}

func TestListProjectFilesEndpoint(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	root := attachmentFixture(t)

	project, err := st.CreateProject("Demo", root, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS("."), "test").Handler())
	defer app.Close()

	get := func(url string) (*http.Response, map[string]any) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		return resp, payload
	}

	resp, payload := get(app.URL + "/api/projects/" + itoa(project.ID) + "/files")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d payload = %v", resp.StatusCode, payload)
	}
	files, _ := payload["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files = %v, want 2 entries", files)
	}
	first, _ := files[0].(map[string]any)
	if first["path"] == "" || first["size_bytes"] == nil {
		t.Errorf("unexpected file entry: %v", first)
	}

	resp, payload = get(app.URL + "/api/projects/" + itoa(project.ID) + "/files?q=UTIL")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d payload = %v", resp.StatusCode, payload)
	}
	files, _ = payload["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("filtered files = %v, want 1 entry", files)
	}

	resp, _ = get(app.URL + "/api/projects/99999/files")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown project status = %d, want 404", resp.StatusCode)
	}
}
