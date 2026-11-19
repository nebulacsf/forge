package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"forge/internal/logx"
	"forge/internal/store"
)

// Agent runs are decoupled from the browser connection. A run owns its own
// context and a sequenced event log; HTTP requests are only viewers that can
// attach, detach and re-attach (after a tab switch or a full page reload)
// without affecting the turn itself.
const (
	// runEventBuffer bounds the replay buffer. When a run outgrows it the oldest
	// events are trimmed and re-attaching viewers are told to reload history.
	runEventBuffer = 20000
	// runRetention is how long a finished run stays replayable.
	runRetention = 10 * time.Minute
	// runKeepAlive is the idle interval of SSE keep-alive comments.
	runKeepAlive = 15 * time.Second
)

// runConflict reports an admission rejection. It is surfaced as HTTP 409.
type runConflict struct{ msg string }

func (e *runConflict) Error() string { return e.msg }

func newRunConflict(format string, args ...any) *runConflict {
	return &runConflict{msg: fmt.Sprintf(format, args...)}
}

// runEvent is one sequenced SSE event of a run.
type runEvent struct {
	Seq uint64
	Ev  chatEvent
}

// run is a single agent turn executing independently of any client.
type run struct {
	reg *runRegistry

	SessionID   int64
	ProjectID   int64
	ProjectName string
	SessionName string
	Mode        string
	StartedAt   time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	events   []runEvent
	base     uint64
	seq      uint64
	round    int
	activity string
	notify   chan struct{}
	finished bool
	reason   string
	stopped  bool
}

func newRun(reg *runRegistry, session store.Session, project store.Project) *run {
	ctx, cancel := context.WithCancel(context.Background())
	title := session.Title
	if strings.TrimSpace(title) == "" {
		title = "New chat"
	}
	return &run{
		reg:         reg,
		SessionID:   session.ID,
		ProjectID:   project.ID,
		ProjectName: project.Name,
		SessionName: title,
		Mode:        session.Mode,
		StartedAt:   time.Now(),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		base:        1,
		notify:      make(chan struct{}),
		activity:    "starting up",
	}
}

// runActivity maps an event type to the short label the run list shows, so the
// UI can say what an agent is doing without opening its chat.
func runActivity(evType string) string {
	switch evType {
	case "round_start":
		return "starting a round"
	case "reasoning":
		return "thinking"
	case "content":
		return "writing"
	case "tool_start":
		return "running a tool"
	case "tool_blocked":
		return "tool blocked"
	case "tool_end":
		return "tool finished"
	case "usage":
		return "reporting usage"
	case "checklist":
		return "updating checklist"
	case "questions":
		return "waiting for your answers"
	case "checkpoint":
		return "checkpoint reached"
	case "limit":
		return "hit a limit"
	}
	return ""
}

// publish appends an event to the run log and wakes every subscriber.
func (r *run) publish(ev chatEvent) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		logx.Debug("event dropped after run finished", "session", r.SessionID, "type", ev.Type)
		return
	}
	r.seq++
	if ev.Type == "round_start" && ev.Round > 0 {
		r.round = ev.Round
	}
	if label := runActivity(ev.Type); label != "" {
		r.activity = label
	}
	if ev.Session != nil && strings.TrimSpace(ev.Session.Title) != "" {
		r.SessionName = ev.Session.Title
	}
	r.events = append(r.events, runEvent{Seq: r.seq, Ev: ev})
	if len(r.events) > runEventBuffer {
		drop := len(r.events) - runEventBuffer
		r.events = append(r.events[:0], r.events[drop:]...)
		r.base = r.events[0].Seq
	}
	signal := r.notify
	r.notify = make(chan struct{})
	r.mu.Unlock()
	close(signal)
}

// finish marks the run complete, frees its admission slot and keeps the event
// log replayable for a while. It is idempotent.
func (r *run) finish(reason string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.reason = reason
	signal := r.notify
	r.notify = nil
	rounds := r.round
	r.mu.Unlock()
	if signal != nil {
		close(signal)
	}
	close(r.done)
	r.cancel()
	r.reg.release(r)
	logx.Info("agent run finished", "session", r.SessionID, "project", r.ProjectID,
		"mode", r.Mode, "reason", reason, "rounds", rounds,
		"duration", time.Since(r.StartedAt).Round(time.Millisecond).String())
}

// requestStop asks the turn to stop. The turn persists its partial output and
// publishes a "cancelled" event before finishing.
func (r *run) requestStop() {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
}

// snapshot returns the buffered events after seq `after`, the first retained
// sequence, the channel to wait on for new events, and the finished flag.
func (r *run) snapshot(after uint64) (events []runEvent, base uint64, notify <-chan struct{}, finished bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	base = r.base
	finished = r.finished
	notify = r.notify
	if len(r.events) > 0 {
		start := 0
		if after != 0 {
			start = sort.Search(len(r.events), func(i int) bool { return r.events[i].Seq > after })
		}
		if start < len(r.events) {
			events = append([]runEvent(nil), r.events[start:]...)
		}
	}
	return events, base, notify, finished
}

// follow replays the log from `after` and then streams live events until the
// run finishes or the viewer goes away. A viewer that fell behind the trimmed
// buffer first receives a "reset" event so it can reload the transcript.
func (r *run) follow(ctx context.Context, after uint64, emit func(runEvent) error, idle func()) error {
	cur := after
	floor := after
	for {
		events, base, notify, finished := r.snapshot(cur)
		if floor < base-1 {
			if err := emit(runEvent{Seq: base - 1, Ev: chatEvent{Type: "reset"}}); err != nil {
				return err
			}
			floor = base - 1
			cur = floor
			continue
		}
		for _, e := range events {
			if err := emit(e); err != nil {
				return err
			}
			cur = e.Seq
			floor = e.Seq
		}
		if finished {
			return nil
		}
		timer := time.NewTimer(runKeepAlive)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-notify:
			timer.Stop()
		case <-timer.C:
			if idle != nil {
				idle()
			}
		}
	}
}

// runStatus is the wire representation of a run for status polling. It carries
// enough context for the "running agents" list to be readable on its own,
// without the client having loaded the project or the chat it belongs to.
type runStatus struct {
	SessionID    int64  `json:"session_id"`
	SessionTitle string `json:"session_title"`
	ProjectID    int64  `json:"project_id"`
	ProjectName  string `json:"project_name"`
	Mode         string `json:"mode"`
	State        string `json:"state"`
	Activity     string `json:"activity"`
	Round        int    `json:"round"`
	StartedAt    string `json:"started_at"`
	Seq          uint64 `json:"seq"`
}

func (r *run) status() runStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "running"
	if r.finished {
		state = "done"
	}
	return runStatus{
		SessionID:    r.SessionID,
		SessionTitle: r.SessionName,
		ProjectID:    r.ProjectID,
		ProjectName:  r.ProjectName,
		Mode:         r.Mode,
		State:        state,
		Activity:     r.activity,
		Round:        r.round,
		StartedAt:    r.StartedAt.UTC().Format(time.RFC3339),
		Seq:          r.seq,
	}
}

func (r *run) lastSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// runRegistry tracks every live run and enforces the per-project admission
// policy: at most one BUILD agent per project, while PLAN agents (read-only)
// may always be added — even alongside a running BUILD.
type runRegistry struct {
	mu       sync.Mutex
	live     map[int64]*run // by session id
	building map[int64]*run // BUILD owner by project id
	recent   map[int64]*run // finished runs kept for replay, by session id
	active   int
	retain   time.Duration
}

func newRunRegistry() *runRegistry {
	return &runRegistry{
		live:     make(map[int64]*run),
		building: make(map[int64]*run),
		recent:   make(map[int64]*run),
		retain:   runRetention,
	}
}

// start admits a new run or explains why it cannot start. limit caps the total
// number of concurrent runs across projects (0 disables the cap).
func (reg *runRegistry) start(session store.Session, project store.Project, limit int) (*run, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, ok := reg.live[session.ID]; ok {
		return nil, newRunConflict("this chat already has a running agent; wait for it or press Stop")
	}
	if session.Mode == "BUILD" {
		if other, ok := reg.building[project.ID]; ok {
			return nil, newRunConflict(
				"%s already has a BUILD agent running (chat #%d) and BUILD writes to the project directory. "+
					"Wait for it to finish, stop it, or switch this chat to PLAN.",
				project.Name, other.SessionID)
		}
	}
	if limit > 0 && reg.active >= limit {
		return nil, newRunConflict("%d agents are already running; wait for one to finish", reg.active)
	}
	rn := newRun(reg, session, project)
	reg.live[session.ID] = rn
	if session.Mode == "BUILD" {
		reg.building[project.ID] = rn
	}
	delete(reg.recent, session.ID)
	reg.active++
	return rn, nil
}

func (reg *runRegistry) release(r *run) {
	reg.mu.Lock()
	if reg.live[r.SessionID] == r {
		delete(reg.live, r.SessionID)
	}
	if reg.building[r.ProjectID] == r {
		delete(reg.building, r.ProjectID)
	}
	reg.active--
	if reg.active < 0 {
		reg.active = 0
	}
	reg.recent[r.SessionID] = r
	reg.mu.Unlock()
	time.AfterFunc(reg.retain, func() {
		reg.mu.Lock()
		if reg.recent[r.SessionID] == r {
			delete(reg.recent, r.SessionID)
		}
		reg.mu.Unlock()
	})
}

// get returns the live run for a session, or a recently finished one so a
// reload can still replay the turn it just missed.
func (reg *runRegistry) get(sessionID int64) *run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if rn, ok := reg.live[sessionID]; ok {
		return rn
	}
	return reg.recent[sessionID]
}

func (reg *runRegistry) liveRun(sessionID int64) *run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.live[sessionID]
}

// stop requests cancellation of a session's run and reports whether one was
// actually running.
func (reg *runRegistry) stop(sessionID int64) bool {
	rn := reg.liveRun(sessionID)
	if rn == nil {
		return false
	}
	rn.requestStop()
	return true
}

// stopAndWait cancels the session run and blocks until it has persisted its
// partial output, so deleting a chat cannot race with its own agent.
func (reg *runRegistry) stopAndWait(sessionID int64, timeout time.Duration) {
	rn := reg.liveRun(sessionID)
	if rn == nil {
		return
	}
	rn.requestStop()
	select {
	case <-rn.done:
	case <-time.After(timeout):
		logx.Warn("run did not finish before timeout", "session", sessionID)
	}
}

func (reg *runRegistry) statuses() []runStatus {
	reg.mu.Lock()
	runs := make([]*run, 0, len(reg.live))
	for _, rn := range reg.live {
		runs = append(runs, rn)
	}
	reg.mu.Unlock()
	out := make([]runStatus, 0, len(runs))
	for _, rn := range runs {
		out = append(out, rn.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

// shutdown cancels every live run and waits for them to persist their state.
func (reg *runRegistry) shutdown(timeout time.Duration) {
	reg.mu.Lock()
	runs := make([]*run, 0, len(reg.live))
	for _, rn := range reg.live {
		runs = append(runs, rn)
	}
	reg.mu.Unlock()
	if len(runs) == 0 {
		return
	}
	logx.Info("cancelling running agents", "count", len(runs))
	for _, rn := range runs {
		rn.requestStop()
	}
	deadline := time.After(timeout)
	for _, rn := range runs {
		select {
		case <-rn.done:
		case <-deadline:
			logx.Warn("shutdown timeout waiting for agents", "pending", len(runs))
			return
		}
	}
}
