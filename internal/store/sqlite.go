// Package store provides Forge's CGO-free SQLite persistence layer.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyAnswered is returned when a question batch has already been answered.
var ErrAlreadyAnswered = errors.New("questions already answered")

type Project struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Path         string  `json:"path"`
	IconColor    string  `json:"icon_color"`
	LastOpenedAt string  `json:"last_opened_at"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	SessionCount int     `json:"session_count"`
}

type Session struct {
	ID           int64   `json:"id"`
	ProjectID    int64   `json:"project_id"`
	Title        string  `json:"title"`
	Model        string  `json:"model"`
	Mode         string  `json:"mode"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	IsOpen       bool    `json:"is_open"`
	UpdatedAt    string  `json:"updated_at"`
}

type Message struct {
	ID         int64  `json:"id"`
	SessionID  int64  `json:"session_id"`
	Role       string `json:"role"`
	Reasoning  string `json:"reasoning"`
	Content    string `json:"content"`
	ToolCalls  string `json:"tool_calls"`
	DurationMS int64  `json:"duration_ms"`
	CreatedAt  string `json:"created_at"`
}

type ChecklistItem struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	TaskText  string `json:"task_text"`
	IsDone    bool   `json:"is_done"`
	Position  int    `json:"position"`
}

// QuestionOption is one selectable answer for a clarifying question.
type QuestionOption struct {
	ID          int64  `json:"id"`
	QuestionID  int64  `json:"question_id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Position    int    `json:"position"`
}

// Question is one clarifying question asked by the agent. AnswerOptionIDs and
// AnswerText are empty until the user answers the batch.
type Question struct {
	ID              int64            `json:"id"`
	BatchID         int64            `json:"batch_id"`
	SessionID       int64            `json:"session_id"`
	Text            string           `json:"text"`
	Context         string           `json:"context"`
	Kind            string           `json:"kind"` // "single" or "multi"
	Position        int              `json:"position"`
	Options         []QuestionOption `json:"options"`
	AnswerOptionIDs []int64          `json:"answer_option_ids"`
	AnswerText      string           `json:"answer_text"`
	AnsweredAt      string           `json:"answered_at,omitempty"`
}

// QuestionBatch groups the questions asked by one ask_user tool call.
type QuestionBatch struct {
	ID         int64      `json:"id"`
	SessionID  int64      `json:"session_id"`
	ToolCallID string     `json:"tool_call_id"`
	CreatedAt  string     `json:"created_at"`
	AnsweredAt string     `json:"answered_at,omitempty"`
	Questions  []Question `json:"questions"`
}

// QuestionOptionInput is one option to persist for a new question.
type QuestionOptionInput struct {
	Label       string
	Description string
}

// QuestionInput is one question to persist for a new batch.
type QuestionInput struct {
	Text    string
	Context string
	Kind    string
	Options []QuestionOptionInput
}

// QuestionAnswerInput is the user's answer to one question.
type QuestionAnswerInput struct {
	QuestionID int64
	OptionIDs  []int64
	Text       string
}

// RepoOutlineFile is one cached file in a project's local repository map.
// Symbols is a compact text form such as "L12 struct User; L30 func New".
type RepoOutlineFile struct {
	ProjectPath string `json:"project_path"`
	FilePath    string `json:"file_path"`
	Language    string `json:"language"`
	MTimeNS     int64  `json:"mtime_ns"`
	SizeBytes   int64  `json:"size_bytes"`
	Symbols     string `json:"symbols"`
	UpdatedAt   string `json:"updated_at"`
}

// ProjectSymbolFile is the per-file freshness marker for the queryable symbol
// index. It exists separately from ProjectSymbol so files with zero symbols are
// still tracked and deletions can be detected.
type ProjectSymbolFile struct {
	ProjectID int64  `json:"project_id"`
	FilePath  string `json:"file_path"`
	Language  string `json:"language"`
	MTimeNS   int64  `json:"mtime_ns"`
	SizeBytes int64  `json:"size_bytes"`
	UpdatedAt string `json:"updated_at"`
}

// ProjectSymbol is one declaration stored for symbol_lookup. It deliberately
// carries no source text: only the metadata needed to navigate to the code.
type ProjectSymbol struct {
	ProjectID     int64  `json:"project_id"`
	FilePath      string `json:"file_path"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Parent        string `json:"parent"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
	// LegacyBackupPath is non-empty when an incompatible older database was
	// moved aside during Open.
	LegacyBackupPath string
}

// Open initializes (or opens) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	s := &Store{}
	legacy, err := inspectLegacy(path)
	if err != nil {
		return nil, err
	}
	if legacy {
		backup := path + ".legacy-" + time.Now().UTC().Format("20060102-150405")
		if err := moveDB(path, backup); err != nil {
			return nil, fmt.Errorf("back up incompatible database: %w", err)
		}
		s.LegacyBackupPath = backup
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	s.db = db
	return s, nil
}

func dsn(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()
	return u.String()
}

func inspectLegacy(path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	defer db.Close()

	var table string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect database schema: %w", err)
	}
	rows, err := db.Query(`PRAGMA table_info(projects)`)
	if err != nil {
		return false, fmt.Errorf("inspect projects table: %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("inspect projects table: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return !cols["path"] || !cols["icon_color"], nil
}

func moveDB(path, backup string) error {
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			_ = os.Rename(path+suffix, backup+suffix)
		}
	}
	return nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS projects (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	path          TEXT NOT NULL UNIQUE,
	icon_color    TEXT NOT NULL DEFAULT '#6c8cff',
	last_opened_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	title      TEXT NOT NULL DEFAULT 'New Chat',
	model      TEXT NOT NULL,
	mode       TEXT NOT NULL DEFAULT 'PLAN' CHECK (mode IN ('PLAN','BUILD')),
	tokens_in  INTEGER NOT NULL DEFAULT 0,
	tokens_out INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd   REAL NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	is_open    INTEGER NOT NULL DEFAULT 1,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	reasoning  TEXT NOT NULL DEFAULT '',
	content    TEXT NOT NULL DEFAULT '',
	tool_calls TEXT,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS checklists (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	task_text  TEXT NOT NULL,
	is_done    INTEGER NOT NULL DEFAULT 0,
	position   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS question_batches (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id   INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	tool_call_id TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	answered_at  TEXT
);

CREATE TABLE IF NOT EXISTS questions (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	batch_id          INTEGER NOT NULL REFERENCES question_batches(id) ON DELETE CASCADE,
	session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	text              TEXT NOT NULL,
	context           TEXT NOT NULL DEFAULT '',
	kind              TEXT NOT NULL DEFAULT 'single' CHECK (kind IN ('single','multi')),
	position          INTEGER NOT NULL DEFAULT 0,
	answer_option_ids TEXT NOT NULL DEFAULT '[]',
	answer_text       TEXT NOT NULL DEFAULT '',
	answered_at       TEXT
);

CREATE TABLE IF NOT EXISTS question_options (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	question_id INTEGER NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	label       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	position    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS repo_outline (
	project_path TEXT NOT NULL,
	file_path    TEXT NOT NULL,
	language     TEXT NOT NULL DEFAULT '',
	mtime_ns     INTEGER NOT NULL DEFAULT 0,
	size_bytes   INTEGER NOT NULL DEFAULT 0,
	symbols      TEXT NOT NULL DEFAULT '',
	updated_at   TEXT NOT NULL,
	PRIMARY KEY (project_path, file_path)
);

CREATE TABLE IF NOT EXISTS project_symbol_files (
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	file_path  TEXT NOT NULL,
	language   TEXT NOT NULL DEFAULT '',
	mtime_ns   INTEGER NOT NULL DEFAULT 0,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (project_id, file_path)
);

CREATE TABLE IF NOT EXISTS project_symbols (
	project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	file_path      TEXT NOT NULL,
	name           TEXT NOT NULL,
	qualified_name TEXT NOT NULL DEFAULT '',
	kind           TEXT NOT NULL,
	parent         TEXT NOT NULL DEFAULT '',
	start_line     INTEGER NOT NULL,
	end_line       INTEGER NOT NULL,
	PRIMARY KEY (project_id, file_path, start_line, name)
);

CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id, is_open, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
CREATE INDEX IF NOT EXISTS idx_checklists_session ON checklists(session_id, position, id);
CREATE INDEX IF NOT EXISTS idx_question_batches_session ON question_batches(session_id, id);
CREATE INDEX IF NOT EXISTS idx_questions_batch ON questions(batch_id, position, id);
CREATE INDEX IF NOT EXISTS idx_question_options_question ON question_options(question_id, position, id);
CREATE INDEX IF NOT EXISTS idx_project_symbols_name ON project_symbols(project_id, name);
CREATE INDEX IF NOT EXISTS idx_project_symbols_kind ON project_symbols(project_id, kind, name);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	// Databases created before cached-token accounting need the new column.
	if err := ensureColumn(db, "sessions", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Turn timers are persisted so session totals survive a reload.
	if err := ensureColumn(db, "sessions", "duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "messages", "duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// ---------------------------------------------------------------------------
// Projects

func (s *Store) CreateProject(name, path, color string) (Project, error) {
	if strings.TrimSpace(name) == "" {
		return Project{}, fmt.Errorf("project name is required")
	}
	if strings.TrimSpace(path) == "" {
		return Project{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(color) == "" {
		color = "#6c8cff"
	}
	ts := now()
	res, err := s.db.Exec(
		`INSERT INTO projects (name, path, icon_color, last_opened_at) VALUES (?, ?, ?, ?)`,
		strings.TrimSpace(name), path, color, ts,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Project{}, fmt.Errorf("this project path is already registered")
		}
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return Project{ID: id, Name: name, Path: path, IconColor: color, LastOpenedAt: ts}, nil
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.name, p.path, p.icon_color, p.last_opened_at,
		       COALESCE((SELECT SUM(s.cost_usd) FROM sessions s WHERE s.project_id = p.id), 0),
		       (SELECT COUNT(*) FROM sessions s WHERE s.project_id = p.id)
		FROM projects p
		ORDER BY p.last_opened_at DESC, p.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.IconColor, &p.LastOpenedAt, &p.TotalCostUSD, &p.SessionCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(id int64) (Project, error) {
	var p Project
	err := s.db.QueryRow(`
		SELECT p.id, p.name, p.path, p.icon_color, p.last_opened_at,
		       COALESCE((SELECT SUM(s.cost_usd) FROM sessions s WHERE s.project_id = p.id), 0),
		       (SELECT COUNT(*) FROM sessions s WHERE s.project_id = p.id)
		FROM projects p WHERE p.id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Path, &p.IconColor, &p.LastOpenedAt, &p.TotalCostUSD, &p.SessionCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) TouchProject(id int64) error {
	_, err := s.db.Exec(`UPDATE projects SET last_opened_at = ? WHERE id = ?`, now(), id)
	return err
}

// UpdateProject applies any non-nil fields and returns the refreshed project.
// Moving a project to a new path drops the stale repository outline cache.
func (s *Store) UpdateProject(id int64, name, path, color *string) (Project, error) {
	current, err := s.GetProject(id)
	if err != nil {
		return Project{}, err
	}
	sets := []string{}
	args := []any{}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" {
			return Project{}, fmt.Errorf("project name cannot be empty")
		}
		sets = append(sets, "name = ?")
		args = append(args, n)
	}
	movedFrom := ""
	if path != nil {
		p := strings.TrimSpace(*path)
		if p == "" {
			return Project{}, fmt.Errorf("project path cannot be empty")
		}
		if p != current.Path {
			movedFrom = current.Path
			sets = append(sets, "path = ?")
			args = append(args, p)
		}
	}
	if color != nil {
		c := strings.TrimSpace(*color)
		if c == "" {
			return Project{}, fmt.Errorf("project color cannot be empty")
		}
		sets = append(sets, "icon_color = ?")
		args = append(args, c)
	}
	if len(sets) == 0 {
		return current, nil
	}
	args = append(args, id)
	if _, err := s.db.Exec(`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Project{}, fmt.Errorf("this project path is already registered")
		}
		return Project{}, err
	}
	if movedFrom != "" {
		_ = s.DeleteRepoOutline(movedFrom)
		_ = s.DeleteProjectSymbols(id)
	}
	return s.GetProject(id)
}

// DeleteProject removes a project along with its sessions, messages, checklists
// and cached repository outline.
func (s *Store) DeleteProject(id int64) error {
	var path string
	err := s.db.QueryRow(`SELECT path FROM projects WHERE id = ?`, id).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_ = s.DeleteRepoOutline(path)
	_ = s.DeleteProjectSymbols(id)
	return nil
}

// ---------------------------------------------------------------------------
// Sessions

func (s *Store) CreateSession(projectID int64, title, model, mode string) (Session, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New Chat"
	}
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode != "BUILD" {
		mode = "PLAN"
	}
	ts := now()
	res, err := s.db.Exec(`
		INSERT INTO sessions (project_id, title, model, mode, is_open, updated_at)
		VALUES (?, ?, ?, ?, 1, ?)`, projectID, title, model, mode, ts)
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return Session{
		ID: id, ProjectID: projectID, Title: title, Model: model, Mode: mode,
		IsOpen: true, UpdatedAt: ts,
	}, nil
}

func (s *Store) ListSessions(projectID int64, openOnly bool) ([]Session, error) {
	query := `SELECT id, project_id, title, model, mode, tokens_in, tokens_out, cached_tokens, cost_usd, duration_ms, is_open, updated_at
		FROM sessions WHERE project_id = ?`
	if openOnly {
		query += ` AND is_open = 1`
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.Query(query, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id int64) (Session, error) {
	row := s.db.QueryRow(`SELECT id, project_id, title, model, mode, tokens_in, tokens_out, cached_tokens, cost_usd, duration_ms, is_open, updated_at
		FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(r rowScanner) (Session, error) {
	var (
		sess   Session
		isOpen int
	)
	if err := r.Scan(&sess.ID, &sess.ProjectID, &sess.Title, &sess.Model, &sess.Mode,
		&sess.TokensIn, &sess.TokensOut, &sess.CachedTokens, &sess.CostUSD, &sess.DurationMS, &isOpen, &sess.UpdatedAt); err != nil {
		return Session{}, err
	}
	sess.IsOpen = isOpen != 0
	return sess, nil
}

// UpdateSession applies any non-nil fields and always touches updated_at.
func (s *Store) UpdateSession(id int64, title, model, mode *string, isOpen *bool) (Session, error) {
	sets := []string{"updated_at = ?"}
	args := []any{now()}
	if title != nil {
		sets = append(sets, "title = ?")
		args = append(args, strings.TrimSpace(*title))
	}
	if model != nil {
		sets = append(sets, "model = ?")
		args = append(args, strings.TrimSpace(*model))
	}
	if mode != nil {
		m := strings.ToUpper(strings.TrimSpace(*mode))
		if m != "PLAN" && m != "BUILD" {
			return Session{}, fmt.Errorf("mode must be PLAN or BUILD")
		}
		sets = append(sets, "mode = ?")
		args = append(args, m)
	}
	if isOpen != nil {
		sets = append(sets, "is_open = ?")
		if *isOpen {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	args = append(args, id)
	res, err := s.db.Exec(`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return Session{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Session{}, ErrNotFound
	}
	return s.GetSession(id)
}

func (s *Store) AddUsage(sessionID, tokensIn, tokensOut, cachedTokens int64, cost float64) error {
	_, err := s.db.Exec(`
		UPDATE sessions
		SET tokens_in = tokens_in + ?, tokens_out = tokens_out + ?,
			cached_tokens = cached_tokens + ?, cost_usd = cost_usd + ?, updated_at = ?
		WHERE id = ?`, tokensIn, tokensOut, cachedTokens, cost, now(), sessionID)
	return err
}

// AddSessionDuration accumulates the wall-clock time spent in agent turns.
func (s *Store) AddSessionDuration(sessionID, durationMS int64) error {
	if durationMS <= 0 {
		return nil
	}
	_, err := s.db.Exec(`
		UPDATE sessions SET duration_ms = duration_ms + ?, updated_at = ? WHERE id = ?`,
		durationMS, now(), sessionID)
	return err
}

func (s *Store) DeleteSession(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ---------------------------------------------------------------------------
// Messages

func (s *Store) AddMessage(sessionID int64, role, reasoning, content, toolCalls string) (Message, error) {
	ts := now()
	res, err := s.db.Exec(`
		INSERT INTO messages (session_id, role, reasoning, content, tool_calls, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, sessionID, role, reasoning, content, toolCalls, ts)
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()
	return Message{
		ID: id, SessionID: sessionID, Role: role, Reasoning: reasoning,
		Content: content, ToolCalls: toolCalls, CreatedAt: ts,
	}, nil
}

func (s *Store) ListMessages(sessionID int64) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, role, reasoning, content, COALESCE(tool_calls, ''), duration_ms, created_at
		FROM messages WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Reasoning, &m.Content, &m.ToolCalls, &m.DurationMS, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMessageDuration records how long the turn that produced a message took.
func (s *Store) SetMessageDuration(id, durationMS int64) error {
	if id == 0 || durationMS < 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE messages SET duration_ms = ? WHERE id = ?`, durationMS, id)
	return err
}

// ReplaceMessages replaces a session's transcript with a summary message
// followed by the kept messages. Used by /compact.
func (s *Store) ReplaceMessages(sessionID int64, summary string, keep []Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	ts := now()
	if _, err := tx.Exec(`
		INSERT INTO messages (session_id, role, reasoning, content, tool_calls, duration_ms, created_at)
		VALUES (?, 'assistant', '', ?, '', 0, ?)`, sessionID, summary, ts); err != nil {
		return err
	}
	for _, m := range keep {
		if strings.TrimSpace(m.Content) == "" && strings.TrimSpace(m.ToolCalls) == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO messages (session_id, role, reasoning, content, tool_calls, duration_ms, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sessionID, m.Role, m.Reasoning, m.Content, m.ToolCalls, m.DurationMS, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Checklists

func (s *Store) ListChecklist(sessionID int64) ([]ChecklistItem, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, task_text, is_done, position
		FROM checklists WHERE session_id = ? ORDER BY position ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChecklistItem{}
	for rows.Next() {
		var (
			it     ChecklistItem
			isDone int
		)
		if err := rows.Scan(&it.ID, &it.SessionID, &it.TaskText, &isDone, &it.Position); err != nil {
			return nil, err
		}
		it.IsDone = isDone != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) GetChecklistItem(sessionID, id int64) (ChecklistItem, error) {
	var (
		it     ChecklistItem
		isDone int
	)
	err := s.db.QueryRow(`
		SELECT id, session_id, task_text, is_done, position
		FROM checklists WHERE session_id = ? AND id = ?`, sessionID, id).
		Scan(&it.ID, &it.SessionID, &it.TaskText, &isDone, &it.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return ChecklistItem{}, ErrNotFound
	}
	it.IsDone = isDone != 0
	return it, err
}

func (s *Store) AddChecklist(sessionID int64, text string) (ChecklistItem, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ChecklistItem{}, fmt.Errorf("task text is required")
	}
	var pos int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(position), -1) + 1 FROM checklists WHERE session_id = ?`, sessionID).Scan(&pos); err != nil {
		return ChecklistItem{}, err
	}
	res, err := s.db.Exec(`INSERT INTO checklists (session_id, task_text, is_done, position) VALUES (?, ?, 0, ?)`,
		sessionID, text, pos)
	if err != nil {
		return ChecklistItem{}, err
	}
	id, _ := res.LastInsertId()
	return ChecklistItem{ID: id, SessionID: sessionID, TaskText: text, Position: pos}, nil
}

func (s *Store) SetChecklistDone(sessionID, id int64, done bool) error {
	v := 0
	if done {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE checklists SET is_done = ? WHERE session_id = ? AND id = ?`, v, sessionID, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateChecklist(sessionID, id int64, text *string, done *bool, position *int) error {
	sets := []string{}
	args := []any{}
	if text != nil {
		sets = append(sets, "task_text = ?")
		args = append(args, strings.TrimSpace(*text))
	}
	if done != nil {
		v := 0
		if *done {
			v = 1
		}
		sets = append(sets, "is_done = ?")
		args = append(args, v)
	}
	if position != nil {
		sets = append(sets, "position = ?")
		args = append(args, *position)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, sessionID, id)
	res, err := s.db.Exec(`UPDATE checklists SET `+strings.Join(sets, ", ")+` WHERE session_id = ? AND id = ?`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChecklist(sessionID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM checklists WHERE session_id = ? AND id = ?`, sessionID, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ClearChecklist(sessionID int64) error {
	_, err := s.db.Exec(`DELETE FROM checklists WHERE session_id = ?`, sessionID)
	return err
}

// ReplaceChecklist resets a checklist to exactly the supplied tasks.
func (s *Store) ReplaceChecklist(sessionID int64, tasks []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM checklists WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	pos := 0
	for _, task := range tasks {
		task = strings.TrimSpace(task)
		if task == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO checklists (session_id, task_text, is_done, position) VALUES (?, ?, 0, ?)`,
			sessionID, task, pos); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Clarifying questions

// CreateQuestionBatch persists one ask_user call as a batch of questions.
func (s *Store) CreateQuestionBatch(sessionID int64, toolCallID string, inputs []QuestionInput) (QuestionBatch, error) {
	if sessionID <= 0 {
		return QuestionBatch{}, fmt.Errorf("session id is required")
	}
	if len(inputs) == 0 {
		return QuestionBatch{}, fmt.Errorf("at least one question is required")
	}
	batch := QuestionBatch{
		SessionID:  sessionID,
		ToolCallID: strings.TrimSpace(toolCallID),
		CreatedAt:  now(),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return QuestionBatch{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`
		INSERT INTO question_batches (session_id, tool_call_id, created_at)
		VALUES (?, ?, ?)`, sessionID, batch.ToolCallID, batch.CreatedAt)
	if err != nil {
		return QuestionBatch{}, err
	}
	batch.ID, _ = res.LastInsertId()
	batch.Questions = make([]Question, 0, len(inputs))
	for pos, input := range inputs {
		text := strings.TrimSpace(input.Text)
		if text == "" {
			return QuestionBatch{}, fmt.Errorf("question text is required")
		}
		kind := strings.ToLower(strings.TrimSpace(input.Kind))
		if kind == "" {
			kind = "single"
		}
		if kind != "single" && kind != "multi" {
			return QuestionBatch{}, fmt.Errorf("question kind must be single or multi")
		}
		context := strings.TrimSpace(input.Context)
		qRes, err := tx.Exec(`
			INSERT INTO questions (batch_id, session_id, text, context, kind, position)
			VALUES (?, ?, ?, ?, ?, ?)`, batch.ID, sessionID, text, context, kind, pos)
		if err != nil {
			return QuestionBatch{}, err
		}
		qID, _ := qRes.LastInsertId()
		q := Question{
			ID: qID, BatchID: batch.ID, SessionID: sessionID,
			Text: text, Context: context, Kind: kind, Position: pos,
			Options: []QuestionOption{},
		}
		for optPos, opt := range input.Options {
			label := strings.TrimSpace(opt.Label)
			if label == "" {
				continue
			}
			description := strings.TrimSpace(opt.Description)
			oRes, err := tx.Exec(`
				INSERT INTO question_options (question_id, label, description, position)
				VALUES (?, ?, ?, ?)`, qID, label, description, optPos)
			if err != nil {
				return QuestionBatch{}, err
			}
			oID, _ := oRes.LastInsertId()
			q.Options = append(q.Options, QuestionOption{
				ID: oID, QuestionID: qID, Label: label, Description: description, Position: optPos,
			})
		}
		batch.Questions = append(batch.Questions, q)
	}
	if err := tx.Commit(); err != nil {
		return QuestionBatch{}, err
	}
	return batch, nil
}

// ListQuestionBatches returns every question batch of a session, oldest first.
func (s *Store) ListQuestionBatches(sessionID int64) ([]QuestionBatch, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, tool_call_id, created_at, COALESCE(answered_at, '')
		FROM question_batches WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batches := []QuestionBatch{}
	byID := map[int64]int{}
	for rows.Next() {
		var b QuestionBatch
		if err := rows.Scan(&b.ID, &b.SessionID, &b.ToolCallID, &b.CreatedAt, &b.AnsweredAt); err != nil {
			return nil, err
		}
		byID[b.ID] = len(batches)
		batches = append(batches, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return batches, nil
	}

	qRows, err := s.db.Query(`
		SELECT id, batch_id, session_id, text, context, kind, position,
		       answer_option_ids, answer_text, COALESCE(answered_at, '')
		FROM questions WHERE session_id = ?
		ORDER BY batch_id ASC, position ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer qRows.Close()
	// Remember where every question landed so its options can be attached
	// after all questions have been read.
	qLoc := map[int64][2]int{}
	for qRows.Next() {
		var (
			q   Question
			raw string
		)
		if err := qRows.Scan(&q.ID, &q.BatchID, &q.SessionID, &q.Text, &q.Context,
			&q.Kind, &q.Position, &raw, &q.AnswerText, &q.AnsweredAt); err != nil {
			return nil, err
		}
		q.AnswerOptionIDs = []int64{}
		if strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &q.AnswerOptionIDs); err != nil {
				q.AnswerOptionIDs = []int64{}
			}
		}
		q.Options = []QuestionOption{}
		bi, ok := byID[q.BatchID]
		if !ok {
			continue
		}
		batches[bi].Questions = append(batches[bi].Questions, q)
		qLoc[q.ID] = [2]int{bi, len(batches[bi].Questions) - 1}
	}
	if err := qRows.Err(); err != nil {
		return nil, err
	}

	oRows, err := s.db.Query(`
		SELECT o.id, o.question_id, o.label, o.description, o.position
		FROM question_options o
		JOIN questions q ON q.id = o.question_id
		WHERE q.session_id = ?
		ORDER BY o.question_id ASC, o.position ASC, o.id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer oRows.Close()
	for oRows.Next() {
		var o QuestionOption
		if err := oRows.Scan(&o.ID, &o.QuestionID, &o.Label, &o.Description, &o.Position); err != nil {
			return nil, err
		}
		if loc, ok := qLoc[o.QuestionID]; ok {
			q := &batches[loc[0]].Questions[loc[1]]
			q.Options = append(q.Options, o)
		}
	}
	return batches, oRows.Err()
}

// AnswerQuestionBatch stores the user's answers and marks the batch answered.
func (s *Store) AnswerQuestionBatch(sessionID, batchID int64, answers []QuestionAnswerInput) (QuestionBatch, error) {
	if sessionID <= 0 || batchID <= 0 {
		return QuestionBatch{}, fmt.Errorf("session and batch ids are required")
	}
	var answeredAt sql.NullString
	err := s.db.QueryRow(`SELECT answered_at FROM question_batches WHERE id = ? AND session_id = ?`,
		batchID, sessionID).Scan(&answeredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return QuestionBatch{}, ErrNotFound
	}
	if err != nil {
		return QuestionBatch{}, err
	}
	if answeredAt.Valid && answeredAt.String != "" {
		return QuestionBatch{}, ErrAlreadyAnswered
	}

	batches, err := s.ListQuestionBatches(sessionID)
	if err != nil {
		return QuestionBatch{}, err
	}
	var batch QuestionBatch
	for _, b := range batches {
		if b.ID == batchID {
			batch = b
			break
		}
	}
	if batch.ID == 0 {
		return QuestionBatch{}, ErrNotFound
	}

	answerByQuestion := make(map[int64]QuestionAnswerInput, len(answers))
	for _, a := range answers {
		if a.QuestionID <= 0 {
			continue
		}
		if _, exists := answerByQuestion[a.QuestionID]; exists {
			return QuestionBatch{}, fmt.Errorf("duplicate answer for question %d", a.QuestionID)
		}
		answerByQuestion[a.QuestionID] = a
	}
	if len(answerByQuestion) != len(batch.Questions) {
		return QuestionBatch{}, fmt.Errorf("every question must be answered")
	}

	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return QuestionBatch{}, err
	}
	defer tx.Rollback()
	// Claim the batch first so two concurrent submissions cannot both write.
	claim, err := tx.Exec(`
		UPDATE question_batches SET answered_at = ?
		WHERE id = ? AND session_id = ? AND answered_at IS NULL`, ts, batchID, sessionID)
	if err != nil {
		return QuestionBatch{}, err
	}
	if n, _ := claim.RowsAffected(); n == 0 {
		return QuestionBatch{}, ErrAlreadyAnswered
	}
	for _, q := range batch.Questions {
		a, ok := answerByQuestion[q.ID]
		if !ok {
			return QuestionBatch{}, fmt.Errorf("question %d is unanswered", q.ID)
		}
		ids := uniquePositiveIDs(a.OptionIDs)
		if q.Kind == "single" && len(ids) > 1 {
			return QuestionBatch{}, fmt.Errorf("question %d accepts only one option", q.ID)
		}
		valid := make(map[int64]bool, len(q.Options))
		for _, o := range q.Options {
			valid[o.ID] = true
		}
		for _, id := range ids {
			if !valid[id] {
				return QuestionBatch{}, fmt.Errorf("option %d does not belong to question %d", id, q.ID)
			}
		}
		text := strings.TrimSpace(a.Text)
		if len(ids) == 0 && text == "" {
			return QuestionBatch{}, fmt.Errorf("question %d needs an option or a custom answer", q.ID)
		}
		raw, err := json.Marshal(ids)
		if err != nil {
			return QuestionBatch{}, err
		}
		if _, err := tx.Exec(`
			UPDATE questions SET answer_option_ids = ?, answer_text = ?, answered_at = ?
			WHERE id = ? AND batch_id = ?`, string(raw), text, ts, q.ID, batchID); err != nil {
			return QuestionBatch{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return QuestionBatch{}, err
	}

	batches, err = s.ListQuestionBatches(sessionID)
	if err != nil {
		return QuestionBatch{}, err
	}
	for _, b := range batches {
		if b.ID == batchID {
			return b, nil
		}
	}
	return QuestionBatch{}, ErrNotFound
}

func uniquePositiveIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// Repository outline cache

// ListRepoOutline returns the cached file outline for a project, ordered by path.
func (s *Store) ListRepoOutline(projectPath string) ([]RepoOutlineFile, error) {
	rows, err := s.db.Query(`
		SELECT project_path, file_path, language, mtime_ns, size_bytes, symbols, updated_at
		FROM repo_outline WHERE project_path = ? ORDER BY file_path ASC`, projectPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RepoOutlineFile{}
	for rows.Next() {
		var f RepoOutlineFile
		if err := rows.Scan(&f.ProjectPath, &f.FilePath, &f.Language, &f.MTimeNS,
			&f.SizeBytes, &f.Symbols, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SyncRepoOutline applies a set of file outline upserts and deletions for a
// project in one transaction. Files that are still fresh should be omitted.
func (s *Store) SyncRepoOutline(projectPath string, upserts []RepoOutlineFile, deleted []string) error {
	if len(upserts) == 0 && len(deleted) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, file := range deleted {
		if _, err := tx.Exec(`DELETE FROM repo_outline WHERE project_path = ? AND file_path = ?`,
			projectPath, file); err != nil {
			return err
		}
	}
	ts := now()
	for _, f := range upserts {
		updated := f.UpdatedAt
		if updated == "" {
			updated = ts
		}
		if _, err := tx.Exec(`
			INSERT INTO repo_outline
				(project_path, file_path, language, mtime_ns, size_bytes, symbols, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_path, file_path) DO UPDATE SET
				language = excluded.language,
				mtime_ns = excluded.mtime_ns,
				size_bytes = excluded.size_bytes,
				symbols = excluded.symbols,
				updated_at = excluded.updated_at`,
			projectPath, f.FilePath, f.Language, f.MTimeNS, f.SizeBytes, f.Symbols, updated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRepoOutline removes every cached file for a project.
func (s *Store) DeleteRepoOutline(projectPath string) error {
	_, err := s.db.Exec(`DELETE FROM repo_outline WHERE project_path = ?`, projectPath)
	return err
}

// ---------------------------------------------------------------------------
// Queryable symbol index

// ListProjectSymbolFiles returns the freshness markers for a project keyed by
// relative file path.
func (s *Store) ListProjectSymbolFiles(projectID int64) (map[string]ProjectSymbolFile, error) {
	rows, err := s.db.Query(`
		SELECT project_id, file_path, language, mtime_ns, size_bytes, updated_at
		FROM project_symbol_files WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ProjectSymbolFile)
	for rows.Next() {
		var f ProjectSymbolFile
		if err := rows.Scan(&f.ProjectID, &f.FilePath, &f.Language, &f.MTimeNS,
			&f.SizeBytes, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out[f.FilePath] = f
	}
	return out, rows.Err()
}

// SyncProjectSymbols applies file freshness updates and symbol replacements in
// one transaction. Every file in files has its old symbols replaced by the rows
// in symbols that belong to it; deleted files are removed entirely.
func (s *Store) SyncProjectSymbols(projectID int64, files []ProjectSymbolFile, symbols []ProjectSymbol, deleted []string) error {
	if len(files) == 0 && len(symbols) == 0 && len(deleted) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	changed := make(map[string]bool, len(files)+len(symbols)+len(deleted))
	for _, f := range files {
		changed[f.FilePath] = true
	}
	for _, sym := range symbols {
		changed[sym.FilePath] = true
	}
	for _, file := range deleted {
		changed[file] = true
	}
	for file := range changed {
		if _, err := tx.Exec(`DELETE FROM project_symbols WHERE project_id = ? AND file_path = ?`,
			projectID, file); err != nil {
			return err
		}
	}

	ts := now()
	for _, file := range deleted {
		if _, err := tx.Exec(`DELETE FROM project_symbol_files WHERE project_id = ? AND file_path = ?`,
			projectID, file); err != nil {
			return err
		}
	}
	for _, f := range files {
		updated := f.UpdatedAt
		if updated == "" {
			updated = ts
		}
		if _, err := tx.Exec(`
			INSERT INTO project_symbol_files
				(project_id, file_path, language, mtime_ns, size_bytes, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, file_path) DO UPDATE SET
				language = excluded.language,
				mtime_ns = excluded.mtime_ns,
				size_bytes = excluded.size_bytes,
				updated_at = excluded.updated_at`,
			projectID, f.FilePath, f.Language, f.MTimeNS, f.SizeBytes, updated); err != nil {
			return err
		}
	}

	stmt, err := tx.Prepare(`
		INSERT INTO project_symbols
			(project_id, file_path, name, qualified_name, kind, parent, start_line, end_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, file_path, start_line, name) DO UPDATE SET
			qualified_name = excluded.qualified_name,
			kind = excluded.kind,
			parent = excluded.parent,
			end_line = excluded.end_line`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sym := range symbols {
		if _, err := stmt.Exec(projectID, sym.FilePath, sym.Name, sym.QualifiedName,
			sym.Kind, sym.Parent, sym.StartLine, sym.EndLine); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SearchProjectSymbols returns declarations whose name or qualified name
// contains query (case-insensitive), optionally restricted to a set of kinds.
func (s *Store) SearchProjectSymbols(projectID int64, query string, kinds []string, limit int) ([]ProjectSymbol, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	sqlText := `
		SELECT project_id, file_path, name, qualified_name, kind, parent, start_line, end_line
		FROM project_symbols
		WHERE project_id = ?
		  AND (instr(lower(name), lower(?)) > 0 OR instr(lower(qualified_name), lower(?)) > 0)`
	args := []any{projectID, query, query}
	if len(kinds) > 0 {
		sqlText += ` AND kind IN (` + placeholders(len(kinds)) + `)`
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	sqlText += `
		ORDER BY CASE
			WHEN lower(name) = lower(?) OR lower(qualified_name) = lower(?) THEN 0
			WHEN instr(lower(name), lower(?)) = 1 OR instr(lower(qualified_name), lower(?)) = 1 THEN 1
			ELSE 2
		END, name ASC, file_path ASC, start_line ASC`
	args = append(args, query, query, query, query)
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectSymbol{}
	for rows.Next() {
		var sym ProjectSymbol
		if err := rows.Scan(&sym.ProjectID, &sym.FilePath, &sym.Name, &sym.QualifiedName,
			&sym.Kind, &sym.Parent, &sym.StartLine, &sym.EndLine); err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

// DeleteProjectSymbols removes every indexed file and symbol for a project.
func (s *Store) DeleteProjectSymbols(projectID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_symbols WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_symbol_files WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	return tx.Commit()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
