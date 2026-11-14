// Package tools implements Forge's native, project-scoped agent tools.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
)

const (
	readOutputLimit  = 25000
	bashOutputLimit  = 12000
	diffOutputLimit  = 25000
	grepOutputLimit  = 25000
	maxFileSize      = 10 << 20
	maxSearchFile    = 2 << 20
	maxReadLines     = 200
	maxReadLineRunes = 400
)

var (
	ansiRE   = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	skipDirs = map[string]bool{
		".git": true, ".hg": true, ".svn": true, "node_modules": true,
		"vendor": true, "dist": true, "build": true, ".venv": true,
		"venv": true, "__pycache__": true, ".idea": true, ".vscode": true,
		".next": true, ".nuxt": true, "target": true, ".cache": true,
	}
)

// FileAction describes a file that the agent read or modified.
type FileAction struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "read" or "modified"
}

// Result is the outcome of a tool invocation.
type Result struct {
	Content          string       `json:"content"`
	IsError          bool         `json:"is_error"`
	ChecklistChanged bool         `json:"checklist_changed"`
	Files            []FileAction `json:"files,omitempty"`
}

// Manager executes tools rooted at a single project directory.
type Manager struct {
	Root        string
	MaxRead     int
	BashTimeout time.Duration
	Store       *store.Store
	SessionID   int64
	Mode        string
}

func NewManager(root string, maxRead int, bashTimeout time.Duration, st *store.Store, sessionID int64) (*Manager, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("project directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", abs)
	}
	if maxRead <= 0 {
		maxRead = readOutputLimit
	}
	if bashTimeout <= 0 {
		bashTimeout = 30 * time.Second
	}
	return &Manager{Root: abs, MaxRead: maxRead, BashTimeout: bashTimeout, Store: st, SessionID: sessionID}, nil
}

var (
	readOnlyTools = map[string]bool{
		"read_file":        true,
		"grep_search":      true,
		"list_directory":   true,
		"git_diff":         true,
		"update_checklist": true,
	}
	buildOnlyTools = map[string]bool{
		"patch_file": true,
		"write_file": true,
		"bash":       true,
	}
)

// Allowed reports whether a tool may run in the given mode. This is the single
// source of truth used both to advertise tools and to gate execution.
func Allowed(mode, name string) bool {
	if readOnlyTools[name] {
		return true
	}
	if buildOnlyTools[name] {
		return strings.EqualFold(mode, "BUILD")
	}
	return false
}

// ModeBlockReason explains why a tool cannot run in the current mode.
func ModeBlockReason(mode, name string) string {
	if buildOnlyTools[name] && !strings.EqualFold(mode, "BUILD") {
		return name + " is blocked in PLAN mode: PLAN is read-only. Switch to BUILD to modify files or run commands."
	}
	if !Allowed(mode, name) {
		return "unknown tool: " + name
	}
	return ""
}

// Specs returns the tool definitions available in a mode.
func Specs(mode string) []llm.Tool {
	tools := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunctionSpec{
				Name: "read_file",
				Description: "Read a bounded window of a UTF-8 text file inside the project. " +
					"start_line and end_line are REQUIRED and at most 200 lines are returned. " +
					"Before reading, locate the target with the repository outline or grep_search; never read blindly. " +
					"Every returned line is prefixed with its real 1-based line number.",
				Parameters: objectSchema(map[string]any{
					"path":       stringProp("Path relative to the project root."),
					"start_line": integerProp("Required 1-based first line to read (must be >= 1)."),
					"end_line":   integerProp("Required 1-based last line, inclusive. At most start_line+199 is returned."),
				}, "path", "start_line", "end_line"),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionSpec{
				Name:        "grep_search",
				Description: "Search file contents with a Go regular expression. Skips binary files and common vendor/build directories. Returns path:line matches.",
				Parameters: objectSchema(map[string]any{
					"pattern":        stringProp("Regular expression to search for."),
					"path":           stringProp("Optional file or directory relative to the project root (default: root)."),
					"glob":           stringProp("Optional file glob such as *.go or internal/**/*.go."),
					"case_sensitive": booleanProp("Match case-sensitively (default false)."),
					"max_results":    integerProp("Maximum matches to return (default 100, max 500)."),
				}, "pattern"),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionSpec{
				Name:        "list_directory",
				Description: "List a directory tree inside the project, with sizes and directory markers.",
				Parameters: objectSchema(map[string]any{
					"path":           stringProp("Optional directory relative to the project root (default root)."),
					"depth":          integerProp("Recursion depth, 1-5 (default 2)."),
					"include_hidden": booleanProp("Include dotfiles (default false)."),
				}),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionSpec{
				Name:        "git_diff",
				Description: "Show the current git diff for the project or a path, including a note about untracked files when the diff is empty.",
				Parameters: objectSchema(map[string]any{
					"path":   stringProp("Optional path relative to the project root."),
					"staged": booleanProp("Show staged changes (git diff --cached)."),
					"stat":   booleanProp("Show diffstat instead of the full patch."),
				}),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionSpec{
				Name:        "update_checklist",
				Description: "Maintain the visible session checklist. Use it to plan multi-step work and keep progress accurate. Actions: add, toggle, remove, clear, set.",
				Parameters: objectSchema(map[string]any{
					"action":    enumProp("Operation to perform.", "add", "toggle", "remove", "clear", "set"),
					"task_text": stringProp("Task text for action=add."),
					"id":        integerProp("Checklist item id for toggle/remove."),
					"done":      booleanProp("Explicit done state for toggle (omit to invert)."),
					"tasks": map[string]any{
						"type":        "array",
						"description": "Replacement task list for action=set.",
						"items":       map[string]any{"type": "string"},
					},
				}, "action"),
			},
		},
	}
	if strings.EqualFold(mode, "BUILD") {
		tools = append(tools,
			llm.Tool{
				Type: "function",
				Function: llm.ToolFunctionSpec{
					Name:        "patch_file",
					Description: "Apply an exact search-and-replace edit to a text file. Prefer this over write_file for small changes to save tokens. The search string must match the file byte-for-byte (except for automatic CRLF normalization).",
					Parameters: objectSchema(map[string]any{
						"path":        stringProp("Path relative to the project root."),
						"search":      stringProp("Exact text to find."),
						"replace":     stringProp("Replacement text."),
						"replace_all": booleanProp("Replace every occurrence (default false; errors when the search is ambiguous)."),
					}, "path", "search", "replace"),
				},
			},
			llm.Tool{
				Type: "function",
				Function: llm.ToolFunctionSpec{
					Name:        "write_file",
					Description: "Create or fully overwrite a text file inside the project. Parent directories are created automatically.",
					Parameters: objectSchema(map[string]any{
						"path":    stringProp("Path relative to the project root."),
						"content": stringProp("Complete file contents."),
					}, "path", "content"),
				},
			},
			llm.Tool{
				Type: "function",
				Function: llm.ToolFunctionSpec{
					Name:        "bash",
					Description: "Run a shell command with bash in the project root. Use for builds, tests, git inspection, and other local commands. Output is truncated and commands are killed on timeout.",
					Parameters: objectSchema(map[string]any{
						"command":     stringProp("Shell command to execute."),
						"timeout_sec": integerProp("Optional timeout in seconds (capped at 600)."),
					}, "command"),
				},
			},
		)
	}
	return tools
}

// Execute runs a tool by name. Unknown tool names and malformed arguments are
// reported as tool errors rather than aborting the stream.
func (m *Manager) Execute(ctx context.Context, name string, rawArgs json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Content: "Tool cancelled: " + err.Error(), IsError: true}
	}
	if len(bytes.TrimSpace(rawArgs)) == 0 {
		rawArgs = json.RawMessage("{}")
	}
	if !Allowed(m.Mode, name) {
		return fail(ModeBlockReason(m.Mode, name))
	}
	switch name {
	case "read_file":
		var a struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.readFile(a.Path, a.StartLine, a.EndLine)
	case "grep_search":
		var a struct {
			Pattern       string `json:"pattern"`
			Path          string `json:"path"`
			Glob          string `json:"glob"`
			CaseSensitive bool   `json:"case_sensitive"`
			MaxResults    int    `json:"max_results"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.grepSearch(ctx, a.Pattern, a.Path, a.Glob, a.CaseSensitive, a.MaxResults)
	case "list_directory":
		var a struct {
			Path          string `json:"path"`
			Depth         int    `json:"depth"`
			IncludeHidden bool   `json:"include_hidden"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.listDirectory(a.Path, a.Depth, a.IncludeHidden)
	case "git_diff":
		var a struct {
			Path   string `json:"path"`
			Staged bool   `json:"staged"`
			Stat   bool   `json:"stat"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.gitDiff(ctx, a.Path, a.Staged, a.Stat)
	case "update_checklist":
		var a struct {
			Action   string   `json:"action"`
			TaskText string   `json:"task_text"`
			ID       int64    `json:"id"`
			Done     *bool    `json:"done"`
			Tasks    []string `json:"tasks"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.updateChecklist(a.Action, a.TaskText, a.ID, a.Done, a.Tasks)
	case "patch_file":
		var a struct {
			Path       string `json:"path"`
			Search     string `json:"search"`
			Replace    string `json:"replace"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.patchFile(a.Path, a.Search, a.Replace, a.ReplaceAll)
	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.writeFile(a.Path, a.Content)
	case "bash":
		var a struct {
			Command    string `json:"command"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return fail("invalid arguments: " + err.Error())
		}
		return m.bash(ctx, a.Command, a.TimeoutSec)
	default:
		return fail("unknown tool: " + name)
	}
}

func fail(msg string) Result {
	return Result{Content: msg, IsError: true}
}

func (m *Manager) resolve(rel string) (string, string, error) {
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	p := filepath.FromSlash(rel)
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(m.Root, p)
	}
	check, err := filepath.Rel(m.Root, abs)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes the project root", rel)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if err := ensureInside(m.Root, real); err != nil {
			return "", "", fmt.Errorf("path %q escapes the project root through a symlink", rel)
		}
		abs = real
	} else {
		// For not-yet-existing files, resolve the nearest existing ancestor so
		// a symlinked parent cannot escape the project root.
		ancestor := filepath.Dir(abs)
		for {
			if real, err := filepath.EvalSymlinks(ancestor); err == nil {
				if err := ensureInside(m.Root, real); err != nil {
					return "", "", fmt.Errorf("path %q escapes the project root through a symlink", rel)
				}
				break
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
		}
	}
	if check == "." {
		check = ""
	}
	return abs, filepath.ToSlash(check), nil
}

func ensureInside(root, p string) error {
	check, err := filepath.Rel(root, p)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes project root")
	}
	return nil
}

func relLabel(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// ---------------------------------------------------------------------------
// read_file

func (m *Manager) readFile(rel string, startLine, endLine int) Result {
	if startLine < 1 || endLine < 1 {
		return fail("read_file: start_line and end_line are required (1-based line numbers)")
	}
	if endLine < startLine {
		return fail("read_file: end_line must be greater than or equal to start_line")
	}
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("read_file: " + err.Error())
	}
	if info.IsDir() {
		return fail("read_file: " + shown + " is a directory; use list_directory")
	}
	if info.Size() > maxFileSize {
		return fail(fmt.Sprintf("read_file: file is too large (%s)", humanSize(info.Size())))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail("read_file: " + err.Error())
	}
	if isBinary(data) {
		return fail("read_file: " + shown + " appears to be binary")
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if total == 0 {
		return fail("read_file: " + shown + " is empty")
	}
	if startLine > total {
		return fail(fmt.Sprintf("read_file: start_line %d is past the end of %s (%d lines)", startLine, shown, total))
	}

	requestedEnd := endLine
	capped := false
	if endLine-startLine+1 > maxReadLines {
		endLine = startLine + maxReadLines - 1
		capped = true
	}
	if endLine > total {
		endLine = total
	}

	limit := m.MaxRead
	if limit < 1 {
		limit = readOutputLimit
	}
	width := len(strconv.Itoa(endLine))

	var body strings.Builder
	used, returned := 0, 0
	charTruncated := false
	for n := startLine; n <= endLine; n++ {
		text := strings.TrimSuffix(lines[n-1], "\r")
		if utf8.RuneCountInString(text) > maxReadLineRunes {
			text = truncateRunes(text, maxReadLineRunes) + "…"
		}
		rendered := fmt.Sprintf("%*d | %s\n", width, n, text)
		need := utf8.RuneCountInString(rendered)
		remaining := limit - used
		if need > remaining {
			if remaining > 0 {
				body.WriteString(truncateRunes(rendered, remaining))
				used += remaining
				returned++
			}
			charTruncated = true
			break
		}
		body.WriteString(rendered)
		used += need
		returned++
	}
	if returned == 0 {
		return fail(fmt.Sprintf("read_file: output limit (%d characters) is too small to show line %d", limit, startLine))
	}
	last := startLine + returned - 1

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s (lines %d-%d of %d) ---\n", shown, startLine, last, total)
	rendered := body.String()
	b.WriteString(rendered)
	if !strings.HasSuffix(rendered, "\n") {
		b.WriteString("\n")
	}
	if capped {
		fmt.Fprintf(&b, "[read_file capped at %d lines: requested %d-%d, returned %d-%d]\n",
			maxReadLines, startLine, requestedEnd, startLine, last)
	}
	if charTruncated {
		fmt.Fprintf(&b, "[output truncated at %d characters; use a narrower range]\n", limit)
	}
	if last < total && !charTruncated {
		fmt.Fprintf(&b, "[continue with start_line=%d]\n", last+1)
	}
	return Result{
		Content: b.String(),
		Files:   []FileAction{{Path: shown, Action: "read"}},
	}
}

// ---------------------------------------------------------------------------
// grep_search

func (m *Manager) grepSearch(ctx context.Context, pattern, rel, glob string, caseSensitive bool, maxResults int) Result {
	if strings.TrimSpace(pattern) == "" {
		return fail("grep_search: pattern is required")
	}
	expr := pattern
	if !caseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("grep_search: invalid regular expression: " + err.Error())
	}
	if maxResults <= 0 {
		maxResults = 100
	}
	if maxResults > 500 {
		maxResults = 500
	}
	var globRE *regexp.Regexp
	if strings.TrimSpace(glob) != "" {
		globRE, err = compileGlob(glob)
		if err != nil {
			return fail("grep_search: invalid glob: " + err.Error())
		}
	}
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("grep_search: " + err.Error())
	}

	var lines []string
	matches := 0
	truncated := false
	searchFile := func(fileAbs, fileRel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := os.Stat(fileAbs)
		if err != nil || fi.IsDir() || fi.Size() > maxSearchFile {
			return nil
		}
		if globRE != nil && !globRE.MatchString(fileRel) {
			return nil
		}
		data, err := os.ReadFile(fileAbs)
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if matches >= maxResults {
				truncated = true
				return io.EOF
			}
			text := strings.TrimSuffix(line, "\r")
			if utf8.RuneCountInString(text) > 500 {
				text = truncateRunes(text, 500) + "…"
			}
			lines = append(lines, fmt.Sprintf("%s:%d: %s", fileRel, i+1, text))
			matches++
		}
		return nil
	}

	if info.IsDir() {
		err = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				if p != abs && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			fileRel, rerr := filepath.Rel(m.Root, p)
			if rerr != nil {
				return nil
			}
			fileRel = filepath.ToSlash(fileRel)
			if err := searchFile(p, fileRel); err != nil {
				if errors.Is(err, io.EOF) {
					return io.EOF
				}
				return err
			}
			return nil
		})
		if err != nil && !errors.Is(err, io.EOF) {
			return fail("grep_search: " + err.Error())
		}
	} else {
		if err := searchFile(abs, shown); err != nil && !errors.Is(err, io.EOF) {
			return fail("grep_search: " + err.Error())
		}
	}

	if len(lines) == 0 {
		return Result{Content: fmt.Sprintf("No matches for /%s/ in %s", pattern, relLabel(shown))}
	}
	body := strings.Join(lines, "\n")
	if utf8.RuneCountInString(body) > grepOutputLimit {
		body = truncateRunes(body, grepOutputLimit) + "\n[output truncated]"
	}
	header := fmt.Sprintf("%d match(es) for /%s/:\n", len(lines), pattern)
	if truncated {
		header = fmt.Sprintf("%d match(es) (limit reached) for /%s/:\n", len(lines), pattern)
	}
	return Result{Content: header + body}
}

// ---------------------------------------------------------------------------
// list_directory

func (m *Manager) listDirectory(rel string, depth int, includeHidden bool) Result {
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("list_directory: " + err.Error())
	}
	if depth <= 0 {
		depth = 2
	}
	if depth > 5 {
		depth = 5
	}
	if !info.IsDir() {
		return Result{Content: fmt.Sprintf("%s (%s)", shown, humanSize(info.Size()))}
	}
	lines := []string{relLabel(shown) + "/"}
	count := 0
	var walk func(dir string, level int) bool
	walk = func(dir string, level int) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
		})
		for _, e := range entries {
			if count >= 2000 {
				lines = append(lines, "  [entry limit reached]")
				return false
			}
			name := e.Name()
			if !includeHidden && strings.HasPrefix(name, ".") {
				continue
			}
			if e.IsDir() && skipDirs[name] {
				continue
			}
			indent := strings.Repeat("  ", level)
			if e.IsDir() {
				lines = append(lines, indent+name+"/")
				count++
				if level < depth && !walk(filepath.Join(dir, name), level+1) {
					return false
				}
			} else {
				size := int64(0)
				if fi, err := e.Info(); err == nil {
					size = fi.Size()
				}
				lines = append(lines, fmt.Sprintf("%s%s (%s)", indent, name, humanSize(size)))
				count++
			}
		}
		return true
	}
	walk(abs, 1)
	return Result{Content: strings.Join(lines, "\n")}
}

// ---------------------------------------------------------------------------
// git_diff

func (m *Manager) gitDiff(ctx context.Context, rel string, staged, stat bool) Result {
	args := []string{"-C", m.Root, "diff", "--no-color"}
	if staged {
		args = append(args, "--cached")
	}
	if stat {
		args = append(args, "--stat")
	}
	if strings.TrimSpace(rel) != "" {
		_, shown, err := m.resolve(rel)
		if err != nil {
			return fail("git_diff: " + err.Error())
		}
		if shown != "" {
			args = append(args, "--", shown)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	configureProcess(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	if err != nil {
		if strings.Contains(text, "not a git repository") {
			return Result{Content: "This project is not a git repository.", IsError: true}
		}
		if ctx.Err() != nil {
			return fail("git_diff: command timed out or was cancelled")
		}
		return fail("git_diff: " + err.Error() + "\n" + text)
	}
	if strings.TrimSpace(text) == "" {
		statusArgs := []string{"-C", m.Root, "status", "--short", "--untracked-files=all"}
		if strings.TrimSpace(rel) != "" {
			_, shown, _ := m.resolve(rel)
			if shown != "" {
				statusArgs = append(statusArgs, "--", shown)
			}
		}
		statusCmd := exec.CommandContext(ctx, "git", statusArgs...)
		configureProcess(statusCmd)
		status, _ := statusCmd.CombinedOutput()
		statusText := strings.TrimSpace(string(status))
		if statusText == "" {
			return Result{Content: "No changes (working tree is clean)."}
		}
		text = "No tracked changes.\n\nGit status:\n" + statusText
	}
	if utf8.RuneCountInString(text) > diffOutputLimit {
		text = truncateRunes(text, diffOutputLimit) + "\n[output truncated]"
	}
	return Result{Content: text}
}

// ---------------------------------------------------------------------------
// checklist

func (m *Manager) updateChecklist(action, taskText string, id int64, done *bool, tasks []string) Result {
	if m.Store == nil || m.SessionID == 0 {
		return fail("update_checklist: checklist is unavailable for this session")
	}
	switch action {
	case "add":
		if strings.TrimSpace(taskText) == "" {
			return fail("update_checklist: task_text is required for add")
		}
		item, err := m.Store.AddChecklist(m.SessionID, taskText)
		if err != nil {
			return fail("update_checklist: " + err.Error())
		}
		return m.checklistResult(fmt.Sprintf("Added checklist item #%d.", item.ID))
	case "toggle":
		if id == 0 {
			return fail("update_checklist: id is required for toggle")
		}
		item, err := m.Store.GetChecklistItem(m.SessionID, id)
		if err != nil {
			return fail("update_checklist: " + err.Error())
		}
		next := !item.IsDone
		if done != nil {
			next = *done
		}
		if err := m.Store.SetChecklistDone(m.SessionID, id, next); err != nil {
			return fail("update_checklist: " + err.Error())
		}
		return m.checklistResult(fmt.Sprintf("Item #%d marked %s.", id, map[bool]string{true: "done", false: "not done"}[next]))
	case "remove":
		if id == 0 {
			return fail("update_checklist: id is required for remove")
		}
		if err := m.Store.DeleteChecklist(m.SessionID, id); err != nil {
			return fail("update_checklist: " + err.Error())
		}
		return m.checklistResult(fmt.Sprintf("Removed checklist item #%d.", id))
	case "clear":
		if err := m.Store.ClearChecklist(m.SessionID); err != nil {
			return fail("update_checklist: " + err.Error())
		}
		return m.checklistResult("Checklist cleared.")
	case "set":
		if err := m.Store.ReplaceChecklist(m.SessionID, tasks); err != nil {
			return fail("update_checklist: " + err.Error())
		}
		return m.checklistResult("Checklist replaced.")
	default:
		return fail("update_checklist: action must be one of add, toggle, remove, clear, set")
	}
}

func (m *Manager) checklistResult(prefix string) Result {
	items, err := m.Store.ListChecklist(m.SessionID)
	if err != nil {
		return fail("update_checklist: " + err.Error())
	}
	var b strings.Builder
	b.WriteString(prefix)
	if len(items) == 0 {
		b.WriteString("\nChecklist is empty.")
	} else {
		b.WriteString("\n")
		for _, it := range items {
			mark := "[ ]"
			if it.IsDone {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "%s #%d %s\n", mark, it.ID, it.TaskText)
		}
	}
	return Result{Content: b.String(), ChecklistChanged: true}
}

// ---------------------------------------------------------------------------
// patch_file

func (m *Manager) patchFile(rel, search, replace string, replaceAll bool) Result {
	if strings.TrimSpace(rel) == "" {
		return fail("patch_file: path is required")
	}
	if search == "" {
		return fail("patch_file: search text is required")
	}
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("patch_file: " + err.Error())
	}
	if info.IsDir() {
		return fail("patch_file: " + shown + " is a directory")
	}
	if info.Size() > maxFileSize {
		return fail("patch_file: file is too large")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail("patch_file: " + err.Error())
	}
	if isBinary(data) {
		return fail("patch_file: " + shown + " appears to be binary")
	}
	content := string(data)
	needle := search
	count := strings.Count(content, needle)
	usedCRLF := false
	if count == 0 && strings.Contains(content, "\r\n") {
		needle = strings.ReplaceAll(search, "\n", "\r\n")
		count = strings.Count(content, needle)
		usedCRLF = count > 0
	}
	if count == 0 {
		return fail("patch_file: search text was not found in " + shown + ". Read the file again and copy the exact text, including whitespace.")
	}
	if count > 1 && !replaceAll {
		return fail(fmt.Sprintf("patch_file: search text occurs %d times in %s; add more context or set replace_all=true", count, shown))
	}
	replacement := replace
	if usedCRLF {
		replacement = strings.ReplaceAll(replace, "\n", "\r\n")
	}
	newContent := strings.Replace(content, needle, replacement, 1)
	replaced := 1
	if replaceAll {
		newContent = strings.ReplaceAll(content, needle, replacement)
		replaced = count
	}
	if err := writeAtomic(abs, []byte(newContent), info.Mode().Perm()); err != nil {
		return fail("patch_file: " + err.Error())
	}
	m.refreshOutline(shown)
	return Result{
		Content: fmt.Sprintf("Patched %s (%d replacement(s), %s).", shown, replaced, humanSize(int64(len(newContent)))),
		Files:   []FileAction{{Path: shown, Action: "modified"}},
	}
}

// ---------------------------------------------------------------------------
// write_file

func (m *Manager) writeFile(rel, content string) Result {
	if strings.TrimSpace(rel) == "" {
		return fail("write_file: path is required")
	}
	if len(content) > maxFileSize {
		return fail("write_file: content exceeds the 10 MB limit")
	}
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return fail(err.Error())
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return fail("write_file: " + shown + " is a directory")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail("write_file: " + err.Error())
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode().Perm()
	}
	if err := writeAtomic(abs, []byte(content), mode); err != nil {
		return fail("write_file: " + err.Error())
	}
	m.refreshOutline(shown)
	lines := 0
	if content != "" {
		lines = strings.Count(content, "\n") + 1
	}
	return Result{
		Content: fmt.Sprintf("Wrote %s (%s, %d lines).", shown, humanSize(int64(len(content))), lines),
		Files:   []FileAction{{Path: shown, Action: "modified"}},
	}
}

// refreshOutline keeps the local repository map current after a write.
func (m *Manager) refreshOutline(rel string) {
	if m.Store == nil || strings.TrimSpace(rel) == "" {
		return
	}
	if err := RefreshOutlineFile(m.Store, m.Root, rel); err != nil {
		logx.Debug("repo outline refresh failed", "file", rel, "error", err)
	}
}

// ---------------------------------------------------------------------------
// bash

func (m *Manager) bash(parent context.Context, command string, timeoutSec int) Result {
	if strings.TrimSpace(command) == "" {
		return fail("bash: command is required")
	}
	timeout := m.BashTimeout
	if timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	if timeout > 600*time.Second {
		timeout = 600 * time.Second
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	logx.Debug("bash exec", "session", m.SessionID, "timeout", timeout.String(),
		"command", logx.Preview(command, 300))
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = m.Root
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "TERM=dumb", "GIT_PAGER=cat", "PAGER=cat", "FORCE_COLOR=0",
	)
	configureProcess(cmd)

	out := &cappedBuffer{limit: 64 * 1024}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()

	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancelled := errors.Is(parent.Err(), context.Canceled)
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if !timedOut && !cancelled {
			return fail("bash: " + err.Error())
		}
	}
	text := strings.TrimRight(ansiRE.ReplaceAllString(out.String(), ""), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", command)
	if timedOut {
		fmt.Fprintf(&b, "[timed out after %s]\n", timeout)
	}
	if cancelled {
		b.WriteString("[cancelled]\n")
	}
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n")
	} else {
		b.WriteString("(no output)\n")
	}
	fmt.Fprintf(&b, "[exit code: %d]", exitCode)
	if out.truncated {
		b.WriteString(" [output capped at 64 KiB]")
	}
	if utf8.RuneCountInString(b.String()) > bashOutputLimit {
		return Result{
			Content: truncateRunes(b.String(), bashOutputLimit) + "\n[output truncated]",
			IsError: err != nil || timedOut || cancelled,
		}
	}
	return Result{Content: b.String(), IsError: err != nil || timedOut || cancelled}
}

// ---------------------------------------------------------------------------
// helpers

func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func booleanProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func enumProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit])
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func writeAtomic(p string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".forge-write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p)
}

func compileGlob(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(strings.TrimSpace(glob))
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	// A bare basename glob should match at any depth.
	if !strings.Contains(glob, "/") {
		base := strings.TrimPrefix(b.String(), "^")
		base = strings.TrimSuffix(base, "$")
		return regexp.Compile(`(^|/)` + base + `$`)
	}
	return re, nil
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	original := len(p)
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		c.truncated = true
		p = p[:remaining]
	}
	_, _ = c.buf.Write(p)
	return original, nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
