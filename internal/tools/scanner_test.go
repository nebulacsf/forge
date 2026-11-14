package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"forge/internal/store"
)

func TestScanGoOutline(t *testing.T) {
	src := `package demo

type User struct {
	Name string
}

type Store interface {
	Save() error
}

func New() *User {
	return &User{}
}

func (u *User) Save() error {
	return nil
}
`
	got := formatOutlineSymbols(scanOutlineFile("go", "demo.go", []byte(src)))
	for _, want := range []string{
		"L1 package demo",
		"L3 struct User",
		"L7 interface Store",
		"L11 func New",
		"L15 method User.Save",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("outline %q missing %q", got, want)
		}
	}
}

func TestScanRegexAndHTMLLanguages(t *testing.T) {
	js := `export class Widget {
  render() {
    return null;
  }
  async load(id) {
    return id;
  }
}
export async function boot() {}
export const load = async (id) => {}
export interface Props {}
`
	got := formatOutlineSymbols(scanOutlineFile("js", "app.js", []byte(js)))
	for _, want := range []string{
		"L1 class Widget", "L2 method render", "L5 method load",
		"L9 func boot", "L10 func load", "L11 type Props",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("js outline %q missing %q", got, want)
		}
	}

	py := `class Widget:
    def render(self):
        pass

def boot():
    pass
`
	got = formatOutlineSymbols(scanOutlineFile("py", "app.py", []byte(py)))
	for _, want := range []string{"L1 class Widget", "L2 method render", "L5 func boot"} {
		if !strings.Contains(got, want) {
			t.Errorf("py outline %q missing %q", got, want)
		}
	}

	html := `<h1>Welcome</h1>
<section id="hero">
`
	got = formatOutlineSymbols(scanOutlineFile("html", "index.html", []byte(html)))
	for _, want := range []string{"L1 heading Welcome", "L2 id section#hero"} {
		if !strings.Contains(got, want) {
			t.Errorf("html outline %q missing %q", got, want)
		}
	}
}

func TestRenderRepoOutlineBudget(t *testing.T) {
	rows := make([]store.RepoOutlineFile, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, store.RepoOutlineFile{
			FilePath: "pkg/file" + strings.Repeat("x", i%20) + ".go",
			Symbols:  "L1 package pkg; L2 func Handler; L10 struct Config; L20 interface Store; L30 method Service.Run",
		})
	}
	out := RenderRepoOutline(rows)
	if n := utf8.RuneCountInString(out); n > outlineMaxRunes {
		t.Errorf("outline is %d runes, want <= %d", n, outlineMaxRunes)
	}
	if !strings.Contains(out, "[outline truncated") {
		t.Errorf("expected truncation notice in %q", out)
	}
}

func TestEnsureRepoOutlineIncremental(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "forge.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	writeFile := func(name, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("main.go", "package main\n\nfunc First() {}\n")
	writeFile("node_modules/ignored.js", "export class Ignored {}\n")
	writeFile("boot.py", "def boot():\n    pass\n")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n\nfunc Leak() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hasSymlink := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "link.go")) == nil

	outline, err := EnsureRepoOutline(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go", "First", "boot.py", "boot"} {
		if !strings.Contains(outline, want) {
			t.Errorf("first outline %q missing %q", outline, want)
		}
	}
	if strings.Contains(outline, "node_modules") || strings.Contains(outline, "Ignored") {
		t.Errorf("outline included skipped directory: %q", outline)
	}
	if hasSymlink && strings.Contains(outline, "secret.go") {
		t.Errorf("outline followed a symlink outside the project: %q", outline)
	}

	// Update one file and make sure only its new symbol appears.
	writeFile("main.go", "package main\n\nfunc First() {}\n\nfunc Second() {}\n")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "main.go"), future, future); err != nil {
		t.Fatal(err)
	}
	outline, err = EnsureRepoOutline(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outline, "Second") {
		t.Errorf("refreshed outline missing new symbol: %q", outline)
	}

	// Deleted files must be pruned from the cache.
	if err := os.Remove(filepath.Join(root, "boot.py")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRepoOutline(ctx, st, root); err != nil {
		t.Fatal(err)
	}
	absRoot, err := outlineRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListRepoOutline(absRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.FilePath == "boot.py" {
			t.Errorf("deleted file remains in cache: %+v", rows)
		}
	}
	if len(rows) != 1 || rows[0].FilePath != "main.go" {
		t.Errorf("rows = %+v, want only main.go", rows)
	}
}
