package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/store"
)

func newTestManager(t *testing.T, mode string, maxRead int) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.go"), []byte("package hello\n\n// greeting is long enough to truncate when the limit is small.\nfunc Greeting() string { return \"hello world\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, maxRead, time.Second, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.Mode = mode
	return m, root
}

func run(t *testing.T, m *Manager, name string, args any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return m.Execute(context.Background(), name, raw)
}

func TestReadFileAndTruncate(t *testing.T) {
	m, _ := newTestManager(t, "PLAN", 120)
	res := run(t, m, "read_file", map[string]any{"path": "hello.go", "start_line": 1, "end_line": 4})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "package hello") {
		t.Errorf("missing content: %q", res.Content)
	}
	if !strings.Contains(res.Content, "1 | package hello") {
		t.Errorf("missing line-number prefix: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[output truncated at 120 characters") {
		t.Errorf("expected truncation notice: %q", res.Content)
	}
	if len(res.Files) != 1 || res.Files[0].Action != "read" || res.Files[0].Path != "hello.go" {
		t.Errorf("files = %+v", res.Files)
	}

	res = run(t, m, "read_file", map[string]any{"path": "blob.bin", "start_line": 1, "end_line": 2})
	if !res.IsError || !strings.Contains(res.Content, "binary") {
		t.Errorf("binary read should fail: %+v", res)
	}
}

func TestReadFileRequiresBoundsAndCapsRange(t *testing.T) {
	m, root := newTestManager(t, "PLAN", 25000)

	res := run(t, m, "read_file", map[string]any{"path": "hello.go"})
	if !res.IsError || !strings.Contains(res.Content, "start_line") {
		t.Errorf("missing bounds should fail: %+v", res)
	}
	res = run(t, m, "read_file", map[string]any{"path": "hello.go", "start_line": 3, "end_line": 2})
	if !res.IsError || !strings.Contains(res.Content, "end_line") {
		t.Errorf("reversed bounds should fail: %+v", res)
	}

	var b strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res = run(t, m, "read_file", map[string]any{"path": "long.txt", "start_line": 1, "end_line": 250})
	if res.IsError {
		t.Fatalf("capped read failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "200 | line 200") {
		t.Errorf("expected line 200 in capped output: %q", res.Content)
	}
	if strings.Contains(res.Content, "201 | line 201") {
		t.Errorf("output exceeded the 200-line cap: %q", res.Content)
	}
	if !strings.Contains(res.Content, "capped at 200 lines") {
		t.Errorf("missing cap notice: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[continue with start_line=201]") {
		t.Errorf("missing continuation hint: %q", res.Content)
	}
}

func TestPathEscapeAndSymlinks(t *testing.T) {
	m, root := newTestManager(t, "BUILD", 25000)
	res := run(t, m, "read_file", map[string]any{"path": "../outside.txt", "start_line": 1, "end_line": 10})
	if !res.IsError {
		t.Fatalf("path escape was allowed: %q", res.Content)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res = run(t, m, "read_file", map[string]any{"path": "escape/secret.txt", "start_line": 1, "end_line": 10})
	if !res.IsError {
		t.Fatalf("symlink escape was allowed: %q", res.Content)
	}
	res = run(t, m, "write_file", map[string]any{"path": "escape/pwn.txt", "content": "nope"})
	if !res.IsError {
		t.Fatalf("write through symlink was allowed: %q", res.Content)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); err == nil {
		t.Fatal("file was written outside the project root")
	}
}

func TestPatchWriteAndPlanGuard(t *testing.T) {
	m, root := newTestManager(t, "BUILD", 25000)
	res := run(t, m, "patch_file", map[string]any{
		"path": "hello.go", "search": `"hello world"`, "replace": `"hi"`,
	})
	if res.IsError {
		t.Fatalf("patch failed: %s", res.Content)
	}
	data, _ := os.ReadFile(filepath.Join(root, "hello.go"))
	if !strings.Contains(string(data), `"hi"`) {
		t.Errorf("patch did not apply: %s", data)
	}

	res = run(t, m, "patch_file", map[string]any{"path": "hello.go", "search": "e", "replace": "x"})
	if !res.IsError || !strings.Contains(res.Content, "occurs") {
		t.Errorf("ambiguous patch should fail: %+v", res)
	}

	res = run(t, m, "write_file", map[string]any{"path": "new/dir/file.txt", "content": "created\n"})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, "new", "dir", "file.txt")); err != nil || string(data) != "created\n" {
		t.Errorf("write result: %q, %v", data, err)
	}

	m.Mode = "PLAN"
	res = run(t, m, "write_file", map[string]any{"path": "blocked.txt", "content": "no"})
	if !res.IsError || !strings.Contains(res.Content, "PLAN") {
		t.Errorf("PLAN should block writes: %+v", res)
	}
}

func TestGrepListAndBash(t *testing.T) {
	m, _ := newTestManager(t, "BUILD", 25000)
	res := run(t, m, "grep_search", map[string]any{"pattern": "Greeting", "glob": "*.go"})
	if res.IsError || !strings.Contains(res.Content, "hello.go:4") {
		t.Errorf("grep result = %+v", res)
	}
	res = run(t, m, "grep_search", map[string]any{"pattern": "content", "glob": "**/*.txt"})
	if res.IsError || !strings.Contains(res.Content, "sub/nested.txt") {
		t.Errorf("grep glob result = %+v", res)
	}
	res = run(t, m, "list_directory", map[string]any{"path": ".", "depth": 2})
	if res.IsError || !strings.Contains(res.Content, "sub/") || !strings.Contains(res.Content, "nested.txt") {
		t.Errorf("list result = %+v", res)
	}
	// Dotfiles are part of a project: a non-git directory shows them.
	if !strings.Contains(res.Content, ".hidden") {
		t.Errorf("list must show dotfiles: %q", res.Content)
	}

	res = run(t, m, "bash", map[string]any{"command": "printf 'ok\\n'"})
	if res.IsError || !strings.Contains(res.Content, "ok") || !strings.Contains(res.Content, "exit code: 0") {
		t.Errorf("bash result = %+v", res)
	}
	res = run(t, m, "bash", map[string]any{"command": "sleep 3", "timeout_sec": 1})
	if !res.IsError || !strings.Contains(strings.ToLower(res.Content), "timed out") {
		t.Errorf("bash timeout result = %+v", res)
	}
}

// TestGrepAndListFollowGitIgnore proves both discovery tools agree with git:
// a tracked dotfile is visible, an ignored file is not, and the explicit
// escape hatches still reach what the filter hides.
func TestGrepAndListFollowGitIgnore(t *testing.T) {
	root := fileSetRepo(t)
	writeProjectFile(t, root, ".github/scripts/release.py", "def publish():\n    pass\n")
	writeProjectFile(t, root, "ignored.py", "def publish_ignored():\n    pass\n")
	writeProjectFile(t, root, "dist/generated.txt", "generated_marker\n")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".env\nignored.py\ndist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".github/scripts/release.py")

	m, err := NewManager(root, 25000, time.Second, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.Mode = "BUILD"

	res := run(t, m, "grep_search", map[string]any{"pattern": "publish"})
	if res.IsError || !strings.Contains(res.Content, ".github/scripts/release.py") {
		t.Errorf("tracked dotfile not searched: %+v", res)
	}
	if strings.Contains(res.Content, "ignored.py") {
		t.Errorf("ignored file was searched: %+v", res)
	}

	res = run(t, m, "list_directory", map[string]any{"path": ".", "depth": 2})
	if res.IsError {
		t.Fatalf("list failed: %+v", res)
	}
	if !strings.Contains(res.Content, ".github/") || !strings.Contains(res.Content, ".gitignore") {
		t.Errorf("list must show tracked dotfiles: %q", res.Content)
	}
	if strings.Contains(res.Content, ".env") || strings.Contains(res.Content, "ignored.py") {
		t.Errorf("list must hide ignored entries: %q", res.Content)
	}
	if !strings.Contains(res.Content, "git-ignored") {
		t.Errorf("list should say how many entries were hidden: %q", res.Content)
	}

	res = run(t, m, "list_directory", map[string]any{"path": ".", "depth": 2, "include_ignored": true})
	if res.IsError || !strings.Contains(res.Content, ".env") {
		t.Errorf("include_ignored must list ignored entries: %+v", res)
	}

	// An explicitly named ignored directory is still listed in full.
	res = run(t, m, "grep_search", map[string]any{"pattern": "publish_ignored"})
	if res.IsError || !strings.Contains(res.Content, "No matches") {
		t.Errorf("ignored file must not match at the root: %+v", res)
	}
}

func TestAllowedByMode(t *testing.T) {
	readOnly := []string{"read_file", "grep_search", "list_directory", "git_diff", "update_checklist", "ask_user"}
	for _, name := range readOnly {
		if !Allowed("PLAN", name) || !Allowed("BUILD", name) {
			t.Errorf("%s should be allowed in both modes", name)
		}
	}
	for _, name := range []string{"patch_file", "write_file", "bash"} {
		if Allowed("PLAN", name) {
			t.Errorf("%s must not be allowed in PLAN", name)
		}
		if !Allowed("BUILD", name) {
			t.Errorf("%s should be allowed in BUILD", name)
		}
		if Allowed("build", name) != true {
			t.Errorf("%s should match BUILD case-insensitively", name)
		}
	}
	if Allowed("PLAN", "made_up_tool") {
		t.Error("unknown tools must not be allowed")
	}
	if !strings.Contains(strings.ToLower(ModeBlockReason("PLAN", "write_file")), "switch to build") {
		t.Errorf("block reason should mention BUILD: %q", ModeBlockReason("PLAN", "write_file"))
	}
}

func TestInvalidRegex(t *testing.T) {
	m, _ := newTestManager(t, "PLAN", 25000)
	res := run(t, m, "grep_search", map[string]any{"pattern": "("})
	if !res.IsError {
		t.Errorf("invalid regex should fail: %+v", res)
	}
}

func newQuestionManager(t *testing.T) (*Manager, *store.Store, store.Session) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	project, err := st.CreateProject("Questions", root, "#6f8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "Questions", "model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, 1000, time.Second, st, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Mode = "PLAN"
	return m, st, session
}

func TestAskUserPersistsQuestions(t *testing.T) {
	m, st, session := newQuestionManager(t)
	raw, err := json.Marshal(map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Which storage should Forge use?",
				"context":  "This decides the migration plan.",
				"type":     "single",
				"options": []any{
					map[string]any{"label": "SQLite", "description": "local file"},
					map[string]any{"label": "Postgres"},
				},
			},
			map[string]any{
				"question": "Which platforms must be supported?",
				"type":     "multi",
				"options": []any{
					map[string]any{"label": "Linux"},
					map[string]any{"label": "macOS"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := m.ExecuteCall(context.Background(), "ask_user", raw, "call_ask_42")
	if res.IsError {
		t.Fatalf("ask_user failed: %s", res.Content)
	}
	if !res.AwaitingAnswer || res.QuestionBatch == nil {
		t.Fatalf("ask_user result = %+v", res)
	}
	batch := res.QuestionBatch
	if batch.ID == 0 || batch.ToolCallID != "call_ask_42" || len(batch.Questions) != 2 {
		t.Fatalf("question batch = %+v", batch)
	}
	if batch.Questions[0].Kind != "single" || batch.Questions[1].Kind != "multi" {
		t.Fatalf("question kinds = %q, %q", batch.Questions[0].Kind, batch.Questions[1].Kind)
	}
	if len(batch.Questions[0].Options) != 2 || batch.Questions[0].Options[0].Description != "local file" {
		t.Fatalf("options = %+v", batch.Questions[0].Options)
	}
	batches, err := st.ListQuestionBatches(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0].ToolCallID != "call_ask_42" || len(batches[0].Questions) != 2 {
		t.Fatalf("persisted batches = %+v", batches)
	}
}

func TestAskUserValidation(t *testing.T) {
	m, _, _ := newQuestionManager(t)
	question := func(text string) map[string]any {
		return map[string]any{"question": text}
	}
	tooManyQuestions := make([]any, 0, maxAskQuestions+1)
	for i := 0; i <= maxAskQuestions; i++ {
		tooManyQuestions = append(tooManyQuestions, question(fmt.Sprintf("Question %d?", i+1)))
	}
	tooManyOptions := make([]any, 0, maxAskOptions+1)
	for i := 0; i <= maxAskOptions; i++ {
		tooManyOptions = append(tooManyOptions, map[string]any{"label": fmt.Sprintf("Option %d", i+1)})
	}
	cases := []struct {
		name      string
		questions []any
		want      string
	}{
		{"empty", []any{}, "at least one question"},
		{"too many questions", tooManyQuestions, "at most 4 questions"},
		{"duplicate questions", []any{question("Same?"), question("Same?")}, "duplicate question"},
		{"empty question", []any{question("   ")}, "is empty"},
		{"invalid type", []any{map[string]any{"question": "Pick one", "type": "many"}}, "type must be single or multi"},
		{"too many options", []any{map[string]any{"question": "Pick one", "options": tooManyOptions}}, "more than 6 options"},
		{"empty option label", []any{map[string]any{"question": "Pick one", "options": []any{map[string]any{"label": "  "}}}}, "empty option label"},
		{"duplicate option labels", []any{map[string]any{"question": "Pick one", "options": []any{
			map[string]any{"label": "A"}, map[string]any{"label": "a"},
		}}}, "duplicate option labels"},
	}
	for _, tc := range cases {
		res := run(t, m, "ask_user", map[string]any{"questions": tc.questions})
		if !res.IsError || !strings.Contains(res.Content, tc.want) {
			t.Errorf("%s: result = %+v, want error containing %q", tc.name, res, tc.want)
		}
	}
}

func TestAskUserRequiresStore(t *testing.T) {
	m, _ := newTestManager(t, "PLAN", 100)
	res := run(t, m, "ask_user", map[string]any{
		"questions": []any{map[string]any{"question": "Anything?"}},
	})
	if !res.IsError || !strings.Contains(res.Content, "unavailable") {
		t.Fatalf("ask_user without a store = %+v", res)
	}
}

func TestAskUserWorksInBuildMode(t *testing.T) {
	m, st, session := newQuestionManager(t)
	m.Mode = "BUILD"
	res := run(t, m, "ask_user", map[string]any{
		"questions": []any{map[string]any{
			"question": "Keep going?",
			"type":     "single",
			"options":  []any{map[string]any{"label": "Yes"}, map[string]any{"label": "No"}},
		}},
	})
	if res.IsError {
		t.Fatalf("ask_user in BUILD failed: %s", res.Content)
	}
	if !res.AwaitingAnswer || res.QuestionBatch == nil {
		t.Fatalf("ask_user in BUILD result = %+v", res)
	}
	batches, err := st.ListQuestionBatches(session.ID)
	if err != nil || len(batches) != 1 {
		t.Fatalf("persisted batches = %+v, %v", batches, err)
	}
}
