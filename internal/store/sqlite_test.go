package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenFreshAndCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p, err := st.CreateProject("Demo", "/tmp/demo", "#22c55e")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Path != "/tmp/demo" {
		t.Fatalf("project = %+v", p)
	}
	if _, err := st.CreateProject("Duplicate", "/tmp/demo", "#000000"); err == nil {
		t.Fatal("duplicate path should be rejected")
	}
	sess, err := st.CreateSession(p.ID, "Chat", "model", "BUILD")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.IsOpen || sess.Mode != "BUILD" {
		t.Fatalf("session = %+v", sess)
	}
	if _, err := st.AddMessage(sess.ID, "user", "", "hello", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUsage(sess.ID, 10, 20, 5, 0.5); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.TokensIn != 10 || fresh.TokensOut != 20 || fresh.CachedTokens != 5 || fresh.CostUSD != 0.5 {
		t.Fatalf("usage not accumulated: %+v", fresh)
	}
	items, err := st.ListMessages(sess.ID)
	if err != nil || len(items) != 1 || items[0].Content != "hello" {
		t.Fatalf("messages = %+v, %v", items, err)
	}
	item, err := st.AddChecklist(sess.ID, "First")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChecklistDone(sess.ID, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceChecklist(sess.ID, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	checklist, err := st.ListChecklist(sess.ID)
	if err != nil || len(checklist) != 2 || checklist[0].TaskText != "A" {
		t.Fatalf("checklist = %+v, %v", checklist, err)
	}
	projects, err := st.ListProjects()
	if err != nil || len(projects) != 1 || projects[0].SessionCount != 1 || projects[0].TotalCostUSD != 0.5 {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
}

func TestRepoOutlineSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	projectPath := "/tmp/outline-project"
	initial := []RepoOutlineFile{
		{ProjectPath: projectPath, FilePath: "main.go", Language: "go", MTimeNS: 10, SizeBytes: 100, Symbols: "L1 package main"},
		{ProjectPath: projectPath, FilePath: "web/app.js", Language: "js", MTimeNS: 20, SizeBytes: 200, Symbols: "L1 func boot"},
	}
	if err := st.SyncRepoOutline(projectPath, initial, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListRepoOutline(projectPath)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].FilePath != "main.go" || rows[1].FilePath != "web/app.js" {
		t.Fatalf("unexpected order: %+v", rows)
	}

	update := []RepoOutlineFile{
		{ProjectPath: projectPath, FilePath: "main.go", Language: "go", MTimeNS: 11, SizeBytes: 120, Symbols: "L1 package main; L5 func main"},
	}
	if err := st.SyncRepoOutline(projectPath, update, []string{"web/app.js"}); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListRepoOutline(projectPath)
	if err != nil || len(rows) != 1 || rows[0].MTimeNS != 11 {
		t.Fatalf("updated rows = %+v, %v", rows, err)
	}

	if err := st.DeleteRepoOutline(projectPath); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListRepoOutline(projectPath)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, %v", rows, err)
	}
}

func TestOpenMovesIncompatibleLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE projects (id INTEGER PRIMARY KEY, root TEXT UNIQUE, name TEXT);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, project_id INTEGER);
		INSERT INTO projects (root, name) VALUES ('/old/root', 'Legacy');`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.LegacyBackupPath == "" {
		t.Fatal("expected a legacy backup path")
	}
	if _, err := os.Stat(st.LegacyBackupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("new database should be empty, got %+v", projects)
	}
	if _, err := st.CreateProject("New", "/new/root", "#123456"); err != nil {
		t.Fatalf("new schema unusable: %v", err)
	}
}

func TestOpenAddsCachedTokensColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	old, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
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
			cost_usd REAL NOT NULL DEFAULT 0,
			is_open INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL
		);`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var count int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'cached_tokens'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cached_tokens column count = %d, want 1", count)
	}
}
