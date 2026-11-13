// Package server exposes Forge's local REST and SSE API plus the embedded UI.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"forge/internal/config"
	"forge/internal/store"
)

const keepAPIKey = "__KEEP__"

var projectPalette = []string{
	"#6c8cff", "#22c55e", "#f59e0b", "#ec4899",
	"#06b6d4", "#a855f7", "#ef4444", "#14b8a6",
}

type Server struct {
	cfg   *config.Manager
	store *store.Store
	web   fs.FS
}

func New(cfg *config.Manager, st *store.Store, web fs.FS) *Server {
	return &Server{cfg: cfg, store: st, web: web}
}

// Handler builds the complete HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)

	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/projects/{id}/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/projects/{id}/sessions", s.handleCreateSession)

	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleListMessages)
	mux.HandleFunc("GET /api/sessions/{id}/checklist", s.handleListChecklist)
	mux.HandleFunc("DELETE /api/sessions/{id}/checklist", s.handleClearChecklist)

	mux.HandleFunc("POST /api/checklist", s.handleCreateChecklist)
	mux.HandleFunc("PATCH /api/checklist/{id}", s.handlePatchChecklist)
	mux.HandleFunc("DELETE /api/checklist/{id}", s.handleDeleteChecklist)

	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)

	fileServer := http.FileServer(http.FS(s.web))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}))

	return recoverMiddleware(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				if strings.HasPrefix(r.URL.Path, "/api/") {
					apiError(w, http.StatusInternalServerError, "internal server error")
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "••••••••"
	}
	return key[:4] + "••••••••" + key[len(key)-4:]
}

func (s *Server) publicConfig() map[string]any {
	cfg := s.cfg.Get()
	set := cfg.Providers.OpenRouter.APIKey != ""
	cfg.Providers.OpenRouter.APIKey = maskAPIKey(cfg.Providers.OpenRouter.APIKey)
	return map[string]any{
		"config":      cfg,
		"path":        s.cfg.Path(),
		"api_key_set": set,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.publicConfig())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var incoming config.Config
	if err := decodeJSON(w, r, &incoming); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	current := s.cfg.Get()
	if incoming.Providers.OpenRouter.APIKey == "" ||
		incoming.Providers.OpenRouter.APIKey == keepAPIKey ||
		strings.Contains(incoming.Providers.OpenRouter.APIKey, "••") {
		incoming.Providers.OpenRouter.APIKey = current.Providers.OpenRouter.APIKey
	}
	if err := s.cfg.Update(incoming); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.publicConfig())
}

// ---------------------------------------------------------------------------
// projects

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Name      string `json:"name"`
		IconColor string `json:"icon_color"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	abs, err := normalizeProjectPath(body.Path)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = filepath.Base(abs)
	}
	color := strings.TrimSpace(body.IconColor)
	if !validColor(color) {
		color = colorForPath(abs)
	}
	project, err := s.store.CreateProject(name, abs, color)
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	projects, _ := s.store.ListProjects()
	writeJSON(w, http.StatusCreated, map[string]any{"project": project, "projects": projects})
}

func normalizeProjectPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot open directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

func validColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func colorForPath(p string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(p))
	return projectPalette[int(h.Sum32())%len(projectPalette)]
}

// ---------------------------------------------------------------------------
// sessions

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetProject(projectID); err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	openOnly := r.URL.Query().Get("all") != "1"
	sessions, err := s.store.ListSessions(projectID, openOnly)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := s.store.GetProject(projectID)
	if err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	var body struct {
		Title string `json:"title"`
		Model string `json:"model"`
		Mode  string `json:"mode"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	cfg := s.cfg.Get()
	if strings.TrimSpace(body.Model) == "" {
		body.Model = cfg.Defaults.Model
	}
	if strings.TrimSpace(body.Mode) == "" {
		body.Mode = cfg.Defaults.Mode
	}
	session, err := s.store.CreateSession(project.ID, body.Title, body.Model, body.Mode)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.TouchProject(project.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"session": session})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	session, err := s.store.GetSession(id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	project, err := s.store.GetProject(session.ProjectID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	messages, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	checklist, err := s.store.ListChecklist(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.TouchProject(project.ID)

	dtos := make([]messageDTO, 0, len(messages))
	for _, m := range messages {
		raw := json.RawMessage(m.ToolCalls)
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("[]")
		}
		dtos = append(dtos, messageDTO{
			ID: m.ID, Role: m.Role, Reasoning: m.Reasoning, Content: m.Content,
			ToolCalls: raw, CreatedAt: m.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session":   session,
		"project":   project,
		"messages":  dtos,
		"checklist": checklist,
	})
}

type messageDTO struct {
	ID        int64           `json:"id"`
	Role      string          `json:"role"`
	Reasoning string          `json:"reasoning"`
	Content   string          `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
	CreatedAt string          `json:"created_at"`
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Title  *string `json:"title"`
		Model  *string `json:"model"`
		Mode   *string `json:"mode"`
		IsOpen *bool   `json:"is_open"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) == "" {
		apiError(w, http.StatusBadRequest, "title cannot be empty")
		return
	}
	if body.Model != nil && strings.TrimSpace(*body.Model) == "" {
		apiError(w, http.StatusBadRequest, "model cannot be empty")
		return
	}
	session, err := s.store.UpdateSession(id, body.Title, body.Model, body.Mode, body.IsOpen)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.DeleteSession(id); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	messages, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]messageDTO, 0, len(messages))
	for _, m := range messages {
		raw := json.RawMessage(m.ToolCalls)
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("[]")
		}
		dtos = append(dtos, messageDTO{ID: m.ID, Role: m.Role, Reasoning: m.Reasoning, Content: m.Content, ToolCalls: raw, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": dtos})
}

// ---------------------------------------------------------------------------
// checklist

func (s *Server) checklistResponse(w http.ResponseWriter, sessionID int64) {
	items, err := s.store.ListChecklist(sessionID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checklist": items})
}

func (s *Server) handleListChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checklistResponse(w, id)
}

func (s *Server) handleCreateChecklist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID int64  `json:"session_id"`
		TaskText  string `json:"task_text"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionID <= 0 {
		apiError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if _, err := s.store.AddChecklist(body.SessionID, body.TaskText); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checklistResponse(w, body.SessionID)
}

func (s *Server) handlePatchChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		SessionID int64   `json:"session_id"`
		TaskText  *string `json:"task_text"`
		IsDone    *bool   `json:"is_done"`
		Position  *int    `json:"position"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionID <= 0 {
		apiError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := s.store.UpdateChecklist(body.SessionID, id, body.TaskText, body.IsDone, body.Position); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "checklist item not found")
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, body.SessionID)
}

func (s *Server) handleDeleteChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID, _ := strconv.ParseInt(r.URL.Query().Get("session_id"), 10, 64)
	if sessionID <= 0 {
		var body struct {
			SessionID int64 `json:"session_id"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, "session_id is required")
			return
		}
		sessionID = body.SessionID
	}
	if err := s.store.DeleteChecklist(sessionID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "checklist item not found")
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, sessionID)
}

func (s *Server) handleClearChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.ClearChecklist(id); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checklistResponse(w, id)
}
