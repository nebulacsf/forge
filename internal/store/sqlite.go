// Package store provides Forge's CGO-free SQLite persistence layer.
package store

import (
	"database/sql"
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
	ID        int64   `json:"id"`
	ProjectID int64   `json:"project_id"`
	Title     string  `json:"title"`
	Model     string  `json:"model"`
	Mode      string  `json:"mode"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	IsOpen    bool    `json:"is_open"`
	UpdatedAt string  `json:"updated_at"`
}

type Message struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Role      string `json:"role"`
	Reasoning string `json:"reasoning"`
	Content   string `json:"content"`
	ToolCalls string `json:"tool_calls"`
	CreatedAt string `json:"created_at"`
}

type ChecklistItem struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	TaskText  string `json:"task_text"`
	IsDone    bool   `json:"is_done"`
	Position  int    `json:"position"`
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
	cost_usd   REAL NOT NULL DEFAULT 0,
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
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS checklists (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	task_text  TEXT NOT NULL,
	is_done    INTEGER NOT NULL DEFAULT 0,
	position   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id, is_open, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
CREATE INDEX IF NOT EXISTS idx_checklists_session ON checklists(session_id, position, id);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
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
	query := `SELECT id, project_id, title, model, mode, tokens_in, tokens_out, cost_usd, is_open, updated_at
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
	row := s.db.QueryRow(`SELECT id, project_id, title, model, mode, tokens_in, tokens_out, cost_usd, is_open, updated_at
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
		&sess.TokensIn, &sess.TokensOut, &sess.CostUSD, &isOpen, &sess.UpdatedAt); err != nil {
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

func (s *Store) AddUsage(sessionID, tokensIn, tokensOut int64, cost float64) error {
	_, err := s.db.Exec(`
		UPDATE sessions
		SET tokens_in = tokens_in + ?, tokens_out = tokens_out + ?, cost_usd = cost_usd + ?, updated_at = ?
		WHERE id = ?`, tokensIn, tokensOut, cost, now(), sessionID)
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
		SELECT id, session_id, role, reasoning, content, COALESCE(tool_calls, ''), created_at
		FROM messages WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Reasoning, &m.Content, &m.ToolCalls, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
