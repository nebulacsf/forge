package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/logx"
	"forge/internal/store"
)

// runPublisher is the agent-side sink. The turn publishes into the run log and
// never touches a socket, so a closed browser can no longer stop a run.
type runPublisher struct{ rn *run }

func (p *runPublisher) send(ev chatEvent) error {
	p.rn.publish(ev)
	return nil
}

func (p *runPublisher) fail(msg string) {
	p.rn.publish(chatEvent{Type: "error", Message: msg})
}

// maxConcurrentRuns is the global admission valve for parallel agents.
func (s *Server) maxConcurrentRuns() int {
	return s.cfg.Get().Defaults.MaxConcurrentRuns
}

// sseStream writes sequenced SSE frames. The sequence is sent as the SSE id so
// a viewer can resume exactly where it left off.
type sseStream struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEStream(w http.ResponseWriter) (*sseStream, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil, err
	}
	return &sseStream{w: w, rc: rc}, nil
}

func (s *sseStream) event(e runEvent) error {
	data, err := json.Marshal(e.Ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "id: %d\ndata: %s\n\n", e.Seq, data); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseStream) ping() {
	if _, err := fmt.Fprint(s.w, ": keep-alive\n\n"); err != nil {
		return
	}
	if err := s.rc.Flush(); err != nil {
		logx.Debug("sse keep-alive failed", "error", err)
	}
}

// tailRun attaches the current HTTP response to a run. Detaching (closing the
// tab, navigating away, losing the network) only ends this viewer: the turn
// keeps running and its events stay replayable.
func (s *Server) tailRun(w http.ResponseWriter, r *http.Request, rn *run, after uint64) {
	out, err := newSSEStream(w)
	if err != nil {
		return
	}
	if id := strings.TrimSpace(r.Header.Get("Last-Event-ID")); after == 0 && id != "" {
		if n, err := strconv.ParseUint(id, 10, 64); err == nil {
			after = n
		}
	}
	if err := rn.follow(r.Context(), after, out.event, out.ping); err != nil && !errors.Is(err, r.Context().Err()) {
		logx.Debug("run viewer detached", "session", rn.SessionID, "error", err)
	}
}

// handleResumeSessionStream re-attaches to the run of a session after a tab
// switch, a reconnect, or a full page reload. It answers 204 when the session
// has neither a live run nor a recently finished one, so the client falls back
// to loading the transcript.
func (s *Server) handleResumeSessionStream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	rn := s.runs.get(id)
	if rn == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var after uint64
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			after = n
		}
	}
	s.tailRun(w, r, rn, after)
}

// handleStopSession cancels the session's run. The turn persists whatever it
// produced before finishing, exactly like a user-initiated stop.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetSession(id); errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	stopped := s.runs.stop(id)
	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped})
}

// handleSessionRun reports the run state of one session so a reloaded client
// can rebuild the tab indicators and re-attach.
func (s *Server) handleSessionRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	rn := s.runs.get(id)
	if rn == nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, rn.status())
}

// handleDismissRun drops the finished entry of a chat from the agents modal.
// The entry disappears once the developer opened that chat, which is the whole
// point of the list: it survives until the result has been looked at.
func (s *Server) handleDismissRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": s.runs.dismiss(id)})
}

// handleListRuns lists the live runs followed by the finished ones the user has
// not opened yet. The UI uses it to know which chats keep working and which
// just produced a result worth reading.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.runs.statuses()})
}

// ShutdownStop cancels every running agent and waits for them to persist their
// partial output. It is called by main before the HTTP server goes down.
func (s *Server) ShutdownStop(timeout time.Duration) {
	s.runs.shutdown(timeout)
}
