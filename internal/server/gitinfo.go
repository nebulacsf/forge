package server

import (
	"net/http"
	"time"

	"forge/internal/logx"
	"forge/internal/tools"
)

// handleProjectGit reports the git state of a project root for the workspace
// bar in the header. The probe is intentionally uncached: the bar refreshes
// after every turn and on demand, and a stale branch after a checkout is worse
// than one short-lived git process.
func (s *Server) handleProjectGit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := s.store.GetProject(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "project not found")
		return
	}
	started := time.Now()
	info := tools.ReadGitInfo(r.Context(), project.Path)
	logx.Debug("project git state", "project", project.ID, "repo", info.IsRepo,
		"branch", info.Branch, "detached", info.Detached,
		"duration", time.Since(started).Round(time.Millisecond).String())
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID,
		"root":       project.Path,
		"is_repo":    info.IsRepo,
		"branch":     info.Branch,
		"detached":   info.Detached,
		"commit":     info.Commit,
	})
}
