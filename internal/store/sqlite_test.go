package store

import (
	"database/sql"
	"errors"
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

func TestProjectSymbolsSyncAndSearch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	project, err := st.CreateProject("Symbols", "/tmp/symbol-project", "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	files := []ProjectSymbolFile{
		{ProjectID: project.ID, FilePath: "main.go", Language: "go", MTimeNS: 10, SizeBytes: 100},
		{ProjectID: project.ID, FilePath: "web/app.js", Language: "js", MTimeNS: 20, SizeBytes: 200},
	}
	symbols := []ProjectSymbol{
		{ProjectID: project.ID, FilePath: "main.go", Name: "User", QualifiedName: "User", Kind: "struct", StartLine: 3, EndLine: 5},
		{ProjectID: project.ID, FilePath: "main.go", Name: "Save", QualifiedName: "User.Save", Kind: "method", Parent: "User", StartLine: 7, EndLine: 9},
		{ProjectID: project.ID, FilePath: "web/app.js", Name: "renderMarkdownInto", QualifiedName: "renderMarkdownInto", Kind: "function", StartLine: 1, EndLine: 8},
		{ProjectID: project.ID, FilePath: "web/app.js", Name: "Widget", QualifiedName: "Widget", Kind: "class", StartLine: 10, EndLine: 20},
	}
	if err := st.SyncProjectSymbols(project.ID, files, symbols, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListProjectSymbolFiles(project.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("files = %+v, %v", got, err)
	}
	if got["main.go"].MTimeNS != 10 || got["web/app.js"].SizeBytes != 200 {
		t.Fatalf("unexpected file metadata: %+v", got)
	}

	matches, err := st.SearchProjectSymbols(project.ID, "save", []string{"method"}, 50)
	if err != nil || len(matches) != 1 || matches[0].QualifiedName != "User.Save" {
		t.Fatalf("method matches = %+v, %v", matches, err)
	}
	matches, err = st.SearchProjectSymbols(project.ID, "widget", []string{"struct", "class"}, 50)
	if err != nil || len(matches) != 1 || matches[0].Kind != "class" {
		t.Fatalf("struct matches = %+v, %v", matches, err)
	}
	matches, err = st.SearchProjectSymbols(project.ID, "RENDER", nil, 50)
	if err != nil || len(matches) != 1 || matches[0].Name != "renderMarkdownInto" {
		t.Fatalf("case-insensitive matches = %+v, %v", matches, err)
	}

	// Replacing a file removes its old symbols; deleting removes the file row.
	update := []ProjectSymbolFile{
		{ProjectID: project.ID, FilePath: "main.go", Language: "go", MTimeNS: 11, SizeBytes: 120},
	}
	replacement := []ProjectSymbol{
		{ProjectID: project.ID, FilePath: "main.go", Name: "User", QualifiedName: "User", Kind: "struct", StartLine: 4, EndLine: 6},
	}
	if err := st.SyncProjectSymbols(project.ID, update, replacement, nil); err != nil {
		t.Fatal(err)
	}
	if matches, err = st.SearchProjectSymbols(project.ID, "Save", nil, 50); err != nil || len(matches) != 0 {
		t.Fatalf("stale method survived sync: %+v, %v", matches, err)
	}
	if err := st.SyncProjectSymbols(project.ID, nil, nil, []string{"main.go"}); err != nil {
		t.Fatal(err)
	}
	got, err = st.ListProjectSymbolFiles(project.ID)
	if err != nil || len(got) != 1 || got["main.go"].FilePath != "" {
		t.Fatalf("deleted file still present: %+v, %v", got, err)
	}
	if err := st.DeleteProjectSymbols(project.ID); err != nil {
		t.Fatal(err)
	}
	if got, err = st.ListProjectSymbolFiles(project.ID); err != nil || len(got) != 0 {
		t.Fatalf("files after delete = %+v, %v", got, err)
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
	for table, column := range map[string]string{"sessions": "duration_ms", "messages": "duration_ms"} {
		if err := st.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s.%s column count = %d, want 1", table, column, count)
		}
	}
}

func TestQuestionBatchesCreateListAnswerAndCascade(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	project, err := st.CreateProject("Questions", "/tmp/questions", "#6f8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Questions", "model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateQuestionBatch(session.ID, "", nil); err == nil {
		t.Fatal("empty question batch should be rejected")
	}
	batch, err := st.CreateQuestionBatch(session.ID, "call_ask_1", []QuestionInput{
		{
			Text: "Which database should Forge use?",
			Kind: "single",
			Options: []QuestionOptionInput{
				{Label: "SQLite", Description: "local file"},
				{Label: "Postgres"},
			},
		},
		{
			Text:    "Which platforms must be supported?",
			Context: "This changes the packaging plan.",
			Kind:    "multi",
			Options: []QuestionOptionInput{{Label: "Linux"}, {Label: "macOS"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.ID == 0 || batch.ToolCallID != "call_ask_1" || len(batch.Questions) != 2 {
		t.Fatalf("batch = %+v", batch)
	}
	q0, q1 := batch.Questions[0], batch.Questions[1]
	if q0.Kind != "single" || len(q0.Options) != 2 || q0.Options[0].Description != "local file" {
		t.Fatalf("single question = %+v", q0)
	}
	if q1.Kind != "multi" || q1.Context == "" || len(q1.Options) != 2 {
		t.Fatalf("multi question = %+v", q1)
	}
	if len(q0.AnswerOptionIDs) != 0 || q0.AnswerText != "" || q0.AnsweredAt != "" {
		t.Fatalf("new question should be unanswered: %+v", q0)
	}

	batches, err := st.ListQuestionBatches(session.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Questions) != 2 {
		t.Fatalf("ListQuestionBatches = %+v, %v", batches, err)
	}
	other, err := st.CreateSession(project.ID, "Other", "model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	if otherBatches, err := st.ListQuestionBatches(other.ID); err != nil || len(otherBatches) != 0 {
		t.Fatalf("other session batches = %+v, %v", otherBatches, err)
	}

	invalid := []struct {
		name    string
		answers []QuestionAnswerInput
	}{
		{"missing question", []QuestionAnswerInput{{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[0].ID}}}},
		{"duplicate answer", []QuestionAnswerInput{
			{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[0].ID}},
			{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[1].ID}},
			{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID}},
		}},
		{"empty answer", []QuestionAnswerInput{
			{QuestionID: q0.ID},
			{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID}},
		}},
		{"two options on single", []QuestionAnswerInput{
			{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[0].ID, q0.Options[1].ID}},
			{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID}},
		}},
		{"foreign option", []QuestionAnswerInput{
			{QuestionID: q0.ID, OptionIDs: []int64{q1.Options[0].ID}},
			{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID}},
		}},
	}
	for _, tc := range invalid {
		if _, err := st.AnswerQuestionBatch(session.ID, batch.ID, tc.answers); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}

	answered, err := st.AnswerQuestionBatch(session.ID, batch.ID, []QuestionAnswerInput{
		{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[0].ID}},
		{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID, q1.Options[1].ID}, Text: "also Windows"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answered.AnsweredAt == "" {
		t.Fatal("batch should be marked answered")
	}
	aq0, aq1 := answered.Questions[0], answered.Questions[1]
	if len(aq0.AnswerOptionIDs) != 1 || aq0.AnswerOptionIDs[0] != q0.Options[0].ID || aq0.AnsweredAt == "" {
		t.Fatalf("answered single question = %+v", aq0)
	}
	if len(aq1.AnswerOptionIDs) != 2 || aq1.AnswerText != "also Windows" || aq1.AnsweredAt == "" {
		t.Fatalf("answered multi question = %+v", aq1)
	}
	if _, err := st.AnswerQuestionBatch(session.ID, batch.ID, []QuestionAnswerInput{
		{QuestionID: q0.ID, OptionIDs: []int64{q0.Options[1].ID}},
		{QuestionID: q1.ID, OptionIDs: []int64{q1.Options[0].ID}},
	}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer error = %v, want ErrAlreadyAnswered", err)
	}
	if _, err := st.AnswerQuestionBatch(session.ID, 999999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown batch error = %v, want ErrNotFound", err)
	}

	if err := st.DeleteSession(session.ID); err != nil {
		t.Fatal(err)
	}
	batches, err = st.ListQuestionBatches(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 0 {
		t.Fatalf("question batches survived session deletion: %+v", batches)
	}
}
