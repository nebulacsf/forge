package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/store"
)

// streamMock emits one content delta, optionally holds the response open until
// the test releases it, then finishes. hold counts how many requests must wait.
type streamMock struct {
	mu     sync.Mutex
	lock   chan struct{}
	hold   int
	chunks []string
}

func newStreamMock(hold int, chunks ...string) *streamMock {
	return &streamMock{lock: make(chan struct{}), hold: hold, chunks: chunks}
}

func (m *streamMock) release() {
	select {
	case <-m.lock:
	default:
		close(m.lock)
	}
}

func (m *streamMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
	}

	m.mu.Lock()
	mustHold := m.hold > 0
	if mustHold {
		m.hold--
	}
	m.mu.Unlock()

	for i, chunk := range m.chunks {
		send(map[string]any{"id": "gen-1", "choices": []any{
			map[string]any{"delta": map[string]any{"content": chunk}, "finish_reason": nil},
		}})
		if i == 0 && mustHold {
			select {
			case <-m.lock:
			case <-r.Context().Done():
				return
			}
		}
	}
	send(map[string]any{"id": "gen-1", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-1", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0.0001,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

// runsFixture builds a server plus one open project and its first session.
func runsFixture(t *testing.T, mode string, mock http.Handler) (*httptest.Server, *Server, *store.Store, store.Session) {
	t.Helper()
	cfg, st, _ := modelsFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(mock)
	t.Cleanup(upstream.Close)

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject("Parallel", dir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", mode)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, os.DirFS("."), "test")
	app := httptest.NewServer(srv.Handler())
	t.Cleanup(app.Close)
	return app, srv, st, session
}

// startChat posts a chat turn without consuming it to completion.
func startChat(t *testing.T, app *httptest.Server, sessionID int64) (*http.Response, func()) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"session_id": sessionID, "content": "work"})
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	return resp, func() { resp.Body.Close() }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func lastAssistant(t *testing.T, st *store.Store, sessionID int64) store.Message {
	t.Helper()
	msgs, err := st.ListMessages(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i]
		}
	}
	t.Fatalf("no assistant message persisted in session %d", sessionID)
	return store.Message{}
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The turn must keep running when the browser goes away: losing the viewer is
// not a cancellation any more.
func TestRunSurvivesClientDisconnect(t *testing.T) {
	mock := newStreamMock(1, "Hello ", "world")
	app, srv, st, session := runsFixture(t, "BUILD", mock)

	resp, closeBody := startChat(t, app, session.ID)
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	closeBody()

	// Detaching must not cancel the run.
	waitFor(t, "viewer detach", func() bool {
		return srv.runs.liveRun(session.ID) != nil
	})
	mock.release()
	waitFor(t, "run completion", func() bool {
		return srv.runs.liveRun(session.ID) == nil
	})

	msg := lastAssistant(t, st, session.ID)
	if !strings.Contains(msg.Content, "Hello") || !strings.Contains(msg.Content, "world") {
		t.Fatalf("assistant message lost part of the response: %q", msg.Content)
	}

	// A reload can still replay the turn it just missed.
	status, run := getJSON(t, app.URL+fmt.Sprintf("/api/sessions/%d/run", session.ID))
	if status != http.StatusOK || run["state"] != "done" {
		t.Fatalf("expected a replayable finished run, got %d %v", status, run)
	}
	resume, err := http.Get(app.URL + fmt.Sprintf("/api/sessions/%d/stream?after=0", session.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resume.Body.Close()
	events := readEvents(t, resume.Body)
	if findEvent(events, "done") == nil {
		t.Fatalf("resume stream did not replay the done event: %v", events)
	}
	if findEvent(events, "content") == nil {
		t.Fatalf("resume stream did not replay content: %v", events)
	}
}

// Only one BUILD agent may run per project, while PLAN agents (read-only) can
// always be added, even next to a running BUILD.
func TestProjectAdmissionPolicy(t *testing.T) {
	mock := newStreamMock(1, "ok")
	app, srv, st, buildSession := runsFixture(t, "BUILD", mock)

	buildResp, closeBuild := startChat(t, app, buildSession.ID)
	defer closeBuild()
	defer buildResp.Body.Close()
	waitFor(t, "build run", func() bool { return srv.runs.liveRun(buildSession.ID) != nil })

	other, err := st.CreateSession(buildSession.ProjectID, "Second build", "mock/model", "BUILD")
	if err != nil {
		t.Fatal(err)
	}
	preflight := func(sessionID int64) (int, string) {
		payload, _ := json.Marshal(map[string]any{"session_id": sessionID, "content": "work"})
		resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.StatusCode, ""
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msg, _ := out["error"].(string)
		return resp.StatusCode, msg
	}

	code, msg := preflight(other.ID)
	if code != http.StatusConflict {
		t.Fatalf("second BUILD in the same project should be rejected, got %d", code)
	}
	if !strings.Contains(msg, "BUILD") {
		t.Errorf("conflict message should explain the BUILD rule, got %q", msg)
	}

	planSession, err := st.CreateSession(buildSession.ProjectID, "Plan", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	planResp, closePlan := startChat(t, app, planSession.ID)
	defer closePlan()
	if planResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(planResp.Body)
		t.Fatalf("PLAN alongside BUILD should be admitted, got %d: %s", planResp.StatusCode, body)
	}
	// The PLAN run completes on its own while BUILD is still held open.
	waitFor(t, "plan run completion", func() bool { return srv.runs.liveRun(planSession.ID) == nil })
	if srv.runs.liveRun(buildSession.ID) == nil {
		t.Fatal("BUILD run should still be running")
	}

	// With only a PLAN run active, a BUILD turn is still exclusive.
	mock.mu.Lock()
	mock.hold = 1
	mock.mu.Unlock()
	plan2, err := st.CreateSession(buildSession.ProjectID, "Plan 2", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	plan2Resp, closePlan2 := startChat(t, app, plan2.ID)
	defer closePlan2()
	defer plan2Resp.Body.Close()
	waitFor(t, "second plan run", func() bool { return srv.runs.liveRun(plan2.ID) != nil })

	if code, _ := preflight(other.ID); code != http.StatusConflict {
		t.Fatalf("BUILD must be exclusive while any agent runs in the project, got %d", code)
	}
	mock.release()
}

// Stopping a run is explicit and still persists what the model produced.
func TestStopEndpointPersistsPartial(t *testing.T) {
	mock := newStreamMock(1, "partial answer")
	app, srv, st, session := runsFixture(t, "BUILD", mock)

	resp, closeBody := startChat(t, app, session.ID)
	defer closeBody()
	// Wait until the streamed text has been published: stopping before the first
	// delta arrives legitimately persists nothing.
	waitFor(t, "first delta", func() bool {
		rn := srv.runs.get(session.ID)
		if rn == nil {
			return false
		}
		events, _, _, _ := rn.snapshot(0)
		for _, e := range events {
			if e.Ev.Type == "content" {
				return true
			}
		}
		return false
	})

	stopResp, err := http.Post(app.URL+fmt.Sprintf("/api/sessions/%d/stop", session.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stopResp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(stopResp.Body).Decode(&out)
	if stopResp.StatusCode != http.StatusOK || out["stopped"] != true {
		t.Fatalf("stop endpoint did not stop the run: %d %v", stopResp.StatusCode, out)
	}
	waitFor(t, "run stop", func() bool { return srv.runs.liveRun(session.ID) == nil })
	_ = resp.Body.Close()

	msg := lastAssistant(t, st, session.ID)
	if !strings.Contains(msg.Content, "partial answer") {
		t.Fatalf("stopped run lost its partial output: %q", msg.Content)
	}
}

// Switching chats while an agent works must not stop it, and the session model
// stays frozen for the duration of the run.
func TestSessionGuardsWhileRunActive(t *testing.T) {
	mock := newStreamMock(1, "ok")
	app, srv, _, session := runsFixture(t, "PLAN", mock)

	resp, closeBody := startChat(t, app, session.ID)
	defer closeBody()
	waitFor(t, "run start", func() bool { return srv.runs.liveRun(session.ID) != nil })

	patch := func(body string) int {
		req, err := http.NewRequest(http.MethodPatch, app.URL+fmt.Sprintf("/api/sessions/%d", session.ID), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if code := patch(`{"mode":"BUILD"}`); code != http.StatusConflict {
		t.Fatalf("mode change during a run should be rejected, got %d", code)
	}
	if code := patch(`{"title":"Renamed"}`); code != http.StatusOK {
		t.Fatalf("renaming during a run should be allowed, got %d", code)
	}
	mock.release()
	waitFor(t, "run completion", func() bool { return srv.runs.liveRun(session.ID) == nil })
	_ = resp.Body.Close()
}

// runsList reads GET /api/runs and returns the entries keyed by session id.
func runsList(t *testing.T, app *httptest.Server) map[int64]map[string]any {
	t.Helper()
	status, out := getJSON(t, app.URL+"/api/runs")
	if status != http.StatusOK {
		t.Fatalf("list runs: %d %v", status, out)
	}
	raw, _ := out["runs"].([]any)
	bySession := make(map[int64]map[string]any, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["session_id"].(float64)
		bySession[int64(id)] = entry
	}
	return bySession
}

// A finished agent must stay listed with the task it worked on: the completion
// toast disappears, and the developer still needs to know what just ended.
func TestFinishedRunStaysListedWithTask(t *testing.T) {
	mock := newStreamMock(0, "done")
	app, srv, _, session := runsFixture(t, "BUILD", mock)

	resp, closeBody := startChat(t, app, session.ID)
	defer closeBody()
	_ = readEvents(t, resp.Body)
	waitFor(t, "run completion", func() bool { return srv.runs.liveRun(session.ID) == nil })

	entry, ok := runsList(t, app)[session.ID]
	if !ok {
		t.Fatal("finished run is missing from /api/runs")
	}
	if entry["state"] != "done" {
		t.Errorf("state = %v, want done", entry["state"])
	}
	if entry["task"] != "work" {
		t.Errorf("task = %v, want the prompt that started the run", entry["task"])
	}
	if entry["finished_at"] == "" || entry["finished_at"] == nil {
		t.Error("finished run has no finished_at")
	}
	if entry["stopped"] == true || entry["failed"] == true {
		t.Errorf("a clean run must not report stopped/failed: %v", entry)
	}

	// Opening the chat is what clears it, both in the list and on the server.
	dismiss, err := http.Post(app.URL+fmt.Sprintf("/api/sessions/%d/run/dismiss", session.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dismiss.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(dismiss.Body).Decode(&out)
	if dismiss.StatusCode != http.StatusOK || out["dismissed"] != true {
		t.Fatalf("dismiss endpoint did not clear the entry: %d %v", dismiss.StatusCode, out)
	}
	if _, ok := runsList(t, app)[session.ID]; ok {
		t.Fatal("dismissed run is still listed")
	}
	// The replay of the turn it produced is untouched.
	if status, run := getJSON(t, app.URL+fmt.Sprintf("/api/sessions/%d/run", session.ID)); status != http.StatusOK || run["state"] != "done" {
		t.Fatalf("dismissing broke replay: %d %v", status, run)
	}

	// Dismissing twice is harmless: the UI fires it whenever a chat is opened.
	again, err := http.Post(app.URL+fmt.Sprintf("/api/sessions/%d/run/dismiss", session.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("second dismiss should be idempotent, got %d", again.StatusCode)
	}
}

// A stopped run is listed as stopped, not as a clean finish.
func TestStoppedRunIsListedAsStopped(t *testing.T) {
	mock := newStreamMock(1, "partial")
	app, srv, _, session := runsFixture(t, "BUILD", mock)

	resp, closeBody := startChat(t, app, session.ID)
	defer closeBody()
	waitFor(t, "first delta", func() bool {
		rn := srv.runs.get(session.ID)
		if rn == nil {
			return false
		}
		events, _, _, _ := rn.snapshot(0)
		for _, e := range events {
			if e.Ev.Type == "content" {
				return true
			}
		}
		return false
	})
	stopResp, err := http.Post(app.URL+fmt.Sprintf("/api/sessions/%d/stop", session.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stopResp.Body.Close()
	waitFor(t, "run stop", func() bool { return srv.runs.liveRun(session.ID) == nil })
	_ = resp.Body.Close()

	entry, ok := runsList(t, app)[session.ID]
	if !ok {
		t.Fatal("stopped run is missing from /api/runs")
	}
	if entry["stopped"] != true {
		t.Errorf("stopped = %v, want true (%v)", entry["stopped"], entry)
	}
	if entry["task"] != "work" {
		t.Errorf("task = %v, want the prompt that started the run", entry["task"])
	}
}

// A new run of the same chat replaces its finished entry instead of piling up.
func TestNewRunReplacesFinishedEntry(t *testing.T) {
	mock := newStreamMock(0, "first")
	app, srv, _, session := runsFixture(t, "PLAN", mock)

	first, closeFirst := startChat(t, app, session.ID)
	_ = readEvents(t, first.Body)
	closeFirst()
	waitFor(t, "first run completion", func() bool { return srv.runs.liveRun(session.ID) == nil })

	payload, _ := json.Marshal(map[string]any{"session_id": session.ID, "content": "second task"})
	second, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	_ = readEvents(t, second.Body)
	waitFor(t, "second run completion", func() bool { return srv.runs.liveRun(session.ID) == nil })

	runs := runsList(t, app)
	entry, ok := runs[session.ID]
	if !ok {
		t.Fatal("second run is missing from /api/runs")
	}
	if entry["task"] != "second task" {
		t.Errorf("task = %v, want the newest prompt", entry["task"])
	}
	count := 0
	for id := range runs {
		if id == session.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one entry per chat, got %d", count)
	}
}

// Deleting the chat must not leave a finished entry pointing at a missing chat.
func TestDeleteSessionForgetsFinishedEntry(t *testing.T) {
	mock := newStreamMock(0, "done")
	app, srv, _, session := runsFixture(t, "BUILD", mock)

	resp, closeBody := startChat(t, app, session.ID)
	_ = readEvents(t, resp.Body)
	closeBody()
	waitFor(t, "run completion", func() bool { return srv.runs.liveRun(session.ID) == nil })
	if _, ok := runsList(t, app)[session.ID]; !ok {
		t.Fatal("finished run is missing from /api/runs")
	}

	req, err := http.NewRequest(http.MethodDelete, app.URL+fmt.Sprintf("/api/sessions/%d", session.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete session: %d", del.StatusCode)
	}
	if entry, ok := runsList(t, app)[session.ID]; ok {
		t.Fatalf("deleted chat still listed as a finished run: %v", entry)
	}
}
