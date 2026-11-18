package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/store"
)

func newSymbolTestManager(t *testing.T) (*Manager, *store.Store, int64, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	project, err := st.CreateProject("Symbols", root, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, 25000, time.Second, st, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.ProjectID = project.ID
	m.Mode = "PLAN"
	return m, st, project.ID, root
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

func touchFuture(t *testing.T, root, rel string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), future, future); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolLookupOutputAndKindFilters(t *testing.T) {
	m, _, _, root := newSymbolTestManager(t)

	writeProjectFile(t, root, "web/js/app.js", `const x = 1;

function renderMarkdownInto(el, raw) {
  try {
    el.innerHTML = sanitizeHtml(raw);
  } catch {
    el.textContent = raw;
  }
}

function sanitizeHtml(html) {
  return html;
}

class Widget {
  render(id) {
    return id;
  }
}
`)

	res := run(t, m, "symbol_lookup", map[string]any{"query": "renderMarkdownInto"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	want := `renderMarkdownInto [function] -> web/js/app.js:3-9 (read_file path="web/js/app.js" start_line=3 end_line=9)`
	if res.Content != want {
		t.Fatalf("output = %q, want %q", res.Content, want)
	}

	res = run(t, m, "symbol_lookup", map[string]any{"query": "sanitize", "kind": "function"})
	if res.IsError || !strings.Contains(res.Content, "sanitizeHtml [function] -> web/js/app.js:11-13") {
		t.Fatalf("function filter = %+v", res)
	}
	if strings.Contains(res.Content, "Widget") {
		t.Fatalf("function filter leaked a class: %q", res.Content)
	}

	res = run(t, m, "symbol_lookup", map[string]any{"query": "widget", "kind": "struct"})
	if res.IsError || !strings.Contains(res.Content, "Widget [class] -> web/js/app.js:15-19") {
		t.Fatalf("struct filter = %+v", res)
	}

	res = run(t, m, "symbol_lookup", map[string]any{"query": "render", "kind": "method"})
	if res.IsError || !strings.Contains(res.Content, "Widget.render [method] -> web/js/app.js:16-18") {
		t.Fatalf("method filter = %+v", res)
	}

	res = run(t, m, "symbol_lookup", map[string]any{"query": "x", "kind": "variable"})
	if res.IsError || !strings.Contains(res.Content, "x [variable] -> web/js/app.js:1-1") {
		t.Fatalf("variable filter = %+v", res)
	}
}

func TestSymbolLookupGoRangesAndLazyReindex(t *testing.T) {
	m, _, _, root := newSymbolTestManager(t)
	writeProjectFile(t, root, "main.go", "package demo\n\nfunc Alpha() {}\n")

	res := run(t, m, "symbol_lookup", map[string]any{"query": "Alpha"})
	if res.IsError || !strings.Contains(res.Content, "Alpha [function] -> main.go:3-3") {
		t.Fatalf("initial lookup = %+v", res)
	}

	// Prepend lines to simulate an edit that shifts every following line.
	writeProjectFile(t, root, "main.go", strings.Repeat("\n", 10)+"package demo\n\nfunc Alpha() {}\n")
	touchFuture(t, root, "main.go")
	res = run(t, m, "symbol_lookup", map[string]any{"query": "Alpha"})
	if res.IsError {
		t.Fatalf("refresh lookup: %s", res.Content)
	}
	if strings.Contains(res.Content, "main.go:3-3") || !strings.Contains(res.Content, "main.go:13-13") {
		t.Fatalf("line drift was not repaired: %q", res.Content)
	}

	// Delete the file and make sure the stale entry disappears.
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	res = run(t, m, "symbol_lookup", map[string]any{"query": "Alpha"})
	if res.IsError || !strings.Contains(res.Content, "No symbols matching") {
		t.Fatalf("deleted file still indexed: %+v", res)
	}

	// A newly created file is indexed on the next lookup.
	writeProjectFile(t, root, "extra.go", "package demo\n\nfunc Beta() {}\n")
	res = run(t, m, "symbol_lookup", map[string]any{"query": "Beta"})
	if res.IsError || !strings.Contains(res.Content, "Beta [function] -> extra.go:3-3") {
		t.Fatalf("new file not indexed: %+v", res)
	}
}

func TestEnsureProjectIndexSkipsUnchangedFiles(t *testing.T) {
	m, _, projectID, root := newSymbolTestManager(t)
	writeProjectFile(t, root, "main.go", "package demo\n\nfunc Alpha() {}\n")
	if _, err := EnsureProjectIndex(context.Background(), m.Store, projectID, root); err != nil {
		t.Fatal(err)
	}
	first, err := m.Store.ListProjectSymbolFiles(projectID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := EnsureProjectIndex(context.Background(), m.Store, projectID, root); err != nil {
		t.Fatal(err)
	}
	second, err := m.Store.ListProjectSymbolFiles(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if first["main.go"].UpdatedAt != second["main.go"].UpdatedAt {
		t.Fatalf("unchanged file was re-indexed: %q -> %q",
			first["main.go"].UpdatedAt, second["main.go"].UpdatedAt)
	}
}

func BenchmarkEnsureProjectIndexWarm(b *testing.B) {
	root := b.TempDir()
	st, err := store.Open(filepath.Join(b.TempDir(), "forge.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	project, err := st.CreateProject("Bench", root, "#6c8cff")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		var sb strings.Builder
		sb.WriteString("package bench\n\n")
		for j := 0; j < 8; j++ {
			fmt.Fprintf(&sb, "func F%d_%d() {}\n", i, j)
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file%03d.go", i)), []byte(sb.String()), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := EnsureProjectIndex(ctx, st, project.ID, root); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EnsureProjectIndex(ctx, st, project.ID, root); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSymbolLookupValidationAndStorage(t *testing.T) {
	m, _, projectID, root := newSymbolTestManager(t)

	res := run(t, m, "symbol_lookup", map[string]any{"query": ""})
	if !res.IsError || !strings.Contains(res.Content, "query is required") {
		t.Fatalf("empty query = %+v", res)
	}

	res = run(t, m, "symbol_lookup", map[string]any{"query": "x", "kind": "banana"})
	if !res.IsError || !strings.Contains(res.Content, "kind must be one of") {
		t.Fatalf("invalid kind = %+v", res)
	}

	m.ProjectID = 0
	res = run(t, m, "symbol_lookup", map[string]any{"query": "x"})
	if !res.IsError || !strings.Contains(res.Content, "unavailable") {
		t.Fatalf("missing project id = %+v", res)
	}
	m.ProjectID = projectID

	writeProjectFile(t, root, "main.go", "package demo\n\nfunc Alpha() {}\n")
	if err := os.Chtimes(filepath.Join(root, "main.go"), time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureProjectIndex(context.Background(), m.Store, projectID, root); err != nil {
		t.Fatal(err)
	}
	files, err := m.Store.ListProjectSymbolFiles(projectID)
	if err != nil || len(files) != 1 {
		t.Fatalf("symbol files = %+v, %v", files, err)
	}
	syms, err := m.Store.SearchProjectSymbols(projectID, "alpha", nil, 10)
	if err != nil || len(syms) != 1 || syms[0].Kind != "function" || syms[0].StartLine != 3 {
		t.Fatalf("symbols = %+v, %v", syms, err)
	}

	if err := m.Store.DeleteProjectSymbols(projectID); err != nil {
		t.Fatal(err)
	}
	files, err = m.Store.ListProjectSymbolFiles(projectID)
	if err != nil || len(files) != 0 {
		t.Fatalf("symbol files after delete = %+v, %v", files, err)
	}
}
