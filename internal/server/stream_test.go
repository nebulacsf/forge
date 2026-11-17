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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/config"
	"forge/internal/store"
)

// mockOpenRouter emits a deterministic two-round tool-calling stream.
type mockOpenRouter struct {
	requests       atomic.Int32
	sawToolMessage atomic.Bool
}

func (m *mockOpenRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := m.requests.Add(1)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.StreamOptions["include_usage"] != true {
		http.Error(w, "missing stream_options.include_usage", http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" ||
		!strings.Contains(req.Messages[0].Content, "switch to BUILD") ||
		!strings.Contains(req.Messages[0].Content, "FIRST action must be update_checklist") ||
		!strings.Contains(req.Messages[0].Content, "Repository outline") ||
		!strings.Contains(req.Messages[0].Content, "GREP-FIRST RULE") ||
		!strings.Contains(req.Messages[0].Content, "Response style (strict)") ||
		!strings.Contains(req.Messages[0].Content, "SHELL PATHS") {
		http.Error(w, "PLAN system prompt missing required guidance", http.StatusBadRequest)
		return
	}
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			m.sawToolMessage.Store(true)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	sendRaw := func(s string) {
		fmt.Fprintf(w, "data: %s\n\n", s)
		flusher.Flush()
	}

	if n > 1 {
		// Give the turn a measurable duration without slowing the test down.
		time.Sleep(20 * time.Millisecond)
	}

	if n == 1 {
		// Verify plan tools are present and build tools are not.
		names := map[string]bool{}
		for _, t := range req.Tools {
			names[t.Function.Name] = true
		}
		if !names["read_file"] || !names["update_checklist"] || names["bash"] {
			http.Error(w, "unexpected plan tool set", http.StatusBadRequest)
			return
		}
		readArgs := `{"path":"main.go","start_line":1,"end_line":50}`
		send(map[string]any{"id": "gen-1", "model": "mock/model", "choices": []any{
			map[string]any{"delta": map[string]any{
				"reasoning": "I should inspect the entry point.",
				"tool_calls": []any{
					map[string]any{"index": 0, "id": "call_read", "type": "function", "function": map[string]any{
						"name": "read_file", "arguments": readArgs[:10],
					}},
				},
			}},
		}})
		send(map[string]any{"id": "gen-1", "choices": []any{
			map[string]any{"delta": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": 0, "function": map[string]any{"arguments": readArgs[10:]}},
					map[string]any{"index": 1, "id": "call_todo", "type": "function", "function": map[string]any{
						"name": "update_checklist", "arguments": `{"action":"add","task_text":"Report the entry point"}`,
					}},
				},
			}},
		}})
		send(map[string]any{"id": "gen-1", "choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		}})
		send(map[string]any{"id": "gen-1", "choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "cost": 0.001,
			"prompt_tokens_details": map[string]any{"cached_tokens": 40},
		}})
		sendRaw("[DONE]")
		return
	}

	send(map[string]any{"id": "gen-2", "choices": []any{
		map[string]any{"delta": map[string]any{"reasoning": "Now summarize."}},
	}})
	send(map[string]any{"id": "gen-2", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "The entry point "}},
	}})
	send(map[string]any{"id": "gen-2", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "prints `hi`."}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-2", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 80, "completion_tokens": 10, "total_tokens": 90, "cost": 0.0005,
	}})
	sendRaw("[DONE]")
}

func TestChatStreamToolLoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FORGE_HOME", home)
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mock := &mockOpenRouter{}
	mockSrv := httptest.NewServer(mock)
	defer mockSrv.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = mockSrv.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(home, "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	project, err := st.CreateProject("Fixture", projectDir, "#22c55e")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	payload, _ := json.Marshal(map[string]any{"session_id": session.ID, "content": "Explain main.go"})
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	events := readEvents(t, resp.Body)
	types := map[string]int{}
	var toolErr, checklistCount, finalContent, usageIn, usageOut, usageCached int
	var sessionEvent *store.Session
	var sawFiles bool
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		types[typ]++
		switch typ {
		case "tool_end":
			if ev["name"] == "read_file" {
				result, _ := ev["result"].(string)
				if !strings.Contains(result, "func main()") {
					t.Errorf("read_file result missing content: %q", result)
				}
				if !strings.Contains(result, "1 | package main") {
					t.Errorf("read_file result missing line numbers: %q", result)
				}
				if isErr, _ := ev["is_error"].(bool); isErr {
					t.Error("read_file unexpectedly errored")
				}
				if files, ok := ev["files"].([]any); ok && len(files) > 0 {
					sawFiles = true
				}
			}
		case "checklist":
			if items, ok := ev["checklist"].([]any); ok {
				checklistCount = len(items)
			}
		case "usage":
			in, _ := ev["session_tokens_in"].(float64)
			out, _ := ev["session_tokens_out"].(float64)
			cached, _ := ev["session_cached_tokens"].(float64)
			usageIn, usageOut, usageCached = int(in), int(out), int(cached)
		case "content":
			if s, ok := ev["content"].(string); ok {
				finalContent += len(s)
			}
		case "error":
			toolErr++
			t.Errorf("stream error event: %v", ev["message"])
		case "done":
			if raw, ok := ev["session"].(map[string]any); ok {
				sessionEvent = &store.Session{
					TokensIn:     int64(raw["tokens_in"].(float64)),
					TokensOut:    int64(raw["tokens_out"].(float64)),
					CachedTokens: int64(raw["cached_tokens"].(float64)),
					CostUSD:      raw["cost_usd"].(float64),
					DurationMS:   int64(raw["duration_ms"].(float64)),
				}
			}
		}
	}
	if types["round_start"] != 2 {
		t.Errorf("round_start count = %d, want 2", types["round_start"])
	}
	if types["reasoning"] < 2 {
		t.Errorf("reasoning events = %d, want >=2", types["reasoning"])
	}
	if types["tool_start"] != 2 || types["tool_end"] != 2 {
		t.Errorf("tool events start/end = %d/%d, want 2/2", types["tool_start"], types["tool_end"])
	}
	if checklistCount != 1 {
		t.Errorf("checklist count = %d, want 1", checklistCount)
	}
	if !sawFiles {
		t.Error("tool_end did not include file actions")
	}
	if finalContent == 0 {
		t.Error("no content events received")
	}
	if usageIn != 180 || usageOut != 30 || usageCached != 40 {
		t.Errorf("usage = %d/%d cached %d, want 180/30/40", usageIn, usageOut, usageCached)
	}
	if sessionEvent == nil || sessionEvent.TokensIn != 180 || sessionEvent.TokensOut != 30 || sessionEvent.CachedTokens != 40 {
		t.Errorf("done session usage = %+v", sessionEvent)
	}
	if !mock.sawToolMessage.Load() {
		t.Error("second provider call did not include tool result messages")
	}
	if toolErr != 0 {
		t.Errorf("saw %d error events", toolErr)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("message count = %d, want 3 (user, assistant tools, assistant final)", len(msgs))
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[1].ToolCalls, `"call_read"`) {
		t.Errorf("unexpected persisted messages: %+v", msgs)
	}
	if msgs[1].DurationMS != 0 {
		t.Errorf("intermediate assistant duration = %d, want 0", msgs[1].DurationMS)
	}
	if msgs[2].DurationMS <= 0 {
		t.Errorf("final assistant duration = %d, want > 0", msgs[2].DurationMS)
	}
	items, _ := st.ListChecklist(session.ID)
	if len(items) != 1 || !strings.Contains(items[0].TaskText, "Report") {
		t.Errorf("checklist not persisted: %+v", items)
	}
	final, _ := st.GetSession(session.ID)
	if final.Title != "Explain main.go" {
		t.Errorf("auto title = %q", final.Title)
	}
	if final.Mode != "PLAN" || final.CostUSD < 0.0014 {
		t.Errorf("session state = %+v", final)
	}
	if final.DurationMS <= 0 {
		t.Errorf("session duration = %d, want > 0", final.DurationMS)
	}
	if sessionEvent == nil || sessionEvent.DurationMS != final.DurationMS {
		t.Errorf("done session duration = %+v, persisted = %d", sessionEvent, final.DurationMS)
	}
	if final.DurationMS != msgs[2].DurationMS {
		t.Errorf("session duration = %d, final message duration = %d; want equal", final.DurationMS, msgs[2].DurationMS)
	}
}

// buildMockOpenRouter verifies BUILD tool exposure and writes a file through
// the agent loop before answering.
type buildMockOpenRouter struct {
	requests       atomic.Int32
	sawToolMessage atomic.Bool
}

func (m *buildMockOpenRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := m.requests.Add(1)
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	names := map[string]bool{}
	for _, t := range req.Tools {
		names[t.Function.Name] = true
	}
	if !names["write_file"] || !names["patch_file"] || !names["bash"] || !names["read_file"] {
		http.Error(w, "unexpected build tool set", http.StatusBadRequest)
		return
	}
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			m.sawToolMessage.Store(true)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	if n == 1 {
		writeArgs := `{"path":"notes/hello.txt","content":"hello forge\n"}`
		bashArgs := `{"command":"cat notes/hello.txt"}`
		send(map[string]any{"id": "gen-b1", "choices": []any{
			map[string]any{"delta": map[string]any{
				"reasoning": "Create the file, then verify it.",
				"tool_calls": []any{
					map[string]any{"index": 0, "id": "call_write", "type": "function", "function": map[string]any{
						"name": "write_file", "arguments": writeArgs[:10],
					}},
				},
			}},
		}})
		send(map[string]any{"id": "gen-b1", "choices": []any{
			map[string]any{"delta": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": 0, "function": map[string]any{"arguments": writeArgs[10:]}},
					map[string]any{"index": 1, "id": "call_bash", "type": "function", "function": map[string]any{
						"name": "bash", "arguments": bashArgs,
					}},
				},
			}},
		}})
		send(map[string]any{"id": "gen-b1", "choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		}})
		send(map[string]any{"id": "gen-b1", "choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 50, "completion_tokens": 12, "total_tokens": 62, "cost": 0.0002,
		}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	send(map[string]any{"id": "gen-b2", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "Created `notes/hello.txt`."}},
	}})
	send(map[string]any{"id": "gen-b2", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-b2", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 40, "completion_tokens": 8, "total_tokens": 48, "cost": 0.0001,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func TestChatStreamBuildTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FORGE_HOME", home)
	projectDir := t.TempDir()

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mock := &buildMockOpenRouter{}
	mockSrv := httptest.NewServer(mock)
	defer mockSrv.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = mockSrv.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(home, "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	project, err := st.CreateProject("Build", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", "BUILD")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	payload, _ := json.Marshal(map[string]any{"session_id": session.ID, "content": "Write a note"})
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	events := readEvents(t, resp.Body)
	var writeOK, bashOK, sawModified bool
	for _, ev := range events {
		if ev["type"] == "error" {
			t.Errorf("stream error event: %v", ev["message"])
		}
		if ev["type"] != "tool_end" {
			continue
		}
		name, _ := ev["name"].(string)
		result, _ := ev["result"].(string)
		if isErr, _ := ev["is_error"].(bool); isErr {
			t.Errorf("tool %s errored: %s", name, result)
		}
		switch name {
		case "write_file":
			writeOK = strings.Contains(result, "Wrote notes/hello.txt")
		case "bash":
			bashOK = strings.Contains(result, "hello forge") && strings.Contains(result, "exit code: 0")
		}
		if files, ok := ev["files"].([]any); ok {
			for _, f := range files {
				if fm, ok := f.(map[string]any); ok && fm["action"] == "modified" {
					sawModified = true
				}
			}
		}
	}
	if !writeOK {
		t.Error("write_file tool_end missing success output")
	}
	if !bashOK {
		t.Error("bash tool_end missing command output")
	}
	if !sawModified {
		t.Error("no modified file action emitted")
	}
	if !mock.sawToolMessage.Load() {
		t.Error("second provider call did not include tool results")
	}

	written, err := os.ReadFile(filepath.Join(projectDir, "notes", "hello.txt"))
	if err != nil {
		t.Fatalf("file was not written: %v", err)
	}
	if string(written) != "hello forge\n" {
		t.Errorf("file content = %q", written)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("message count = %d, want 3", len(msgs))
	}
	if !strings.Contains(msgs[1].ToolCalls, `"call_write"`) || !strings.Contains(msgs[1].ToolCalls, `"status":"done"`) {
		t.Errorf("tool calls not persisted: %s", msgs[1].ToolCalls)
	}
	final, _ := st.GetSession(session.ID)
	if final.Mode != "BUILD" || final.TokensIn != 90 {
		t.Errorf("session state = %+v", final)
	}
}

// blockedToolMock asks for write_file in PLAN mode, then answers.
type blockedToolMock struct {
	requests atomic.Int32
}

func (m *blockedToolMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := m.requests.Add(1)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 || !strings.Contains(req.Messages[0].Content, "Allowed tools in PLAN mode") {
		http.Error(w, "system prompt missing allowed tools", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if n == 1 {
		send(map[string]any{"id": "gen-x1", "choices": []any{
			map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_write", "type": "function", "function": map[string]any{
					"name": "write_file", "arguments": `{"path":"blocked.txt","content":"nope"}`,
				}},
			}}},
		}})
		send(map[string]any{"id": "gen-x1", "choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		}})
		send(map[string]any{"id": "gen-x1", "choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 20, "completion_tokens": 5, "total_tokens": 25, "cost": 0.0001,
		}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	send(map[string]any{"id": "gen-x2", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "PLAN is read-only; switch to BUILD to write files."}},
	}})
	send(map[string]any{"id": "gen-x2", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-x2", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 30, "completion_tokens": 10, "total_tokens": 40, "cost": 0.0001,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func TestChatStreamBlocksToolBeforeExecution(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	projectDir := t.TempDir()
	upstream := httptest.NewServer(&blockedToolMock{})
	defer upstream.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject("Blocked", projectDir, "#22c55e")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	payload, _ := json.Marshal(map[string]any{"session_id": session.ID, "content": "Create a file"})
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := readEvents(t, resp.Body)

	var blocked, started, ended int
	var reason string
	for _, ev := range events {
		switch ev["type"] {
		case "tool_blocked":
			blocked++
			reason, _ = ev["message"].(string)
			if ev["name"] != "write_file" {
				t.Errorf("blocked tool = %v, want write_file", ev["name"])
			}
		case "tool_start":
			started++
		case "tool_end":
			ended++
		}
	}
	if blocked != 1 || started != 0 || ended != 0 {
		t.Errorf("blocked/started/ended = %d/%d/%d, want 1/0/0", blocked, started, ended)
	}
	if !strings.Contains(reason, "PLAN") || !strings.Contains(reason, "BUILD") {
		t.Errorf("block reason = %q", reason)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "blocked.txt")); !os.IsNotExist(err) {
		t.Error("blocked write_file still created the file")
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if !strings.Contains(msgs[1].ToolCalls, `"status":"blocked"`) {
		t.Errorf("blocked status not persisted: %s", msgs[1].ToolCalls)
	}
}

// summaryMock answers /compact with a fixed summary.
type summaryMock struct {
	requests atomic.Int32
	sawTools atomic.Bool
}

func (m *summaryMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []any `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Tools) > 0 {
		m.sawTools.Store(true)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	send(map[string]any{"id": "gen-c1", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "Goals: add health endpoint. Files: main.go. Next: implement handler."}},
	}})
	send(map[string]any{"id": "gen-c1", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-c1", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 500, "completion_tokens": 60, "total_tokens": 560, "cost": 0.002,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func TestCompactSession(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	mock := &summaryMock{}
	upstream := httptest.NewServer(mock)
	defer upstream.Close()
	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject("Compact", t.TempDir(), "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ role, text string }{
		{"user", "First question"},
		{"assistant", "First answer"},
		{"user", "Second question"},
		{"assistant", "Second answer"},
		{"user", "Latest question"},
		{"assistant", "Latest answer"},
	} {
		if _, err := st.AddMessage(session.ID, m.role, "", m.text, ""); err != nil {
			t.Fatal(err)
		}
	}
	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	resp, err := http.Post(app.URL+"/api/sessions/"+strconv.FormatInt(session.ID, 10)+"/compact", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Removed int    `json:"removed"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Removed != 4 {
		t.Errorf("removed = %d, want 4", payload.Removed)
	}
	if len(payload.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (summary + kept pair)", len(payload.Messages))
	}
	if payload.Messages[0].Role != "assistant" || !strings.HasPrefix(payload.Messages[0].Content, "Conversation summary") {
		t.Errorf("first message is not the summary: %+v", payload.Messages[0])
	}
	if payload.Messages[1].Content != "Latest question" || payload.Messages[2].Content != "Latest answer" {
		t.Errorf("kept exchange wrong: %+v", payload.Messages[1:])
	}
	if !strings.Contains(payload.Summary, "health endpoint") {
		t.Errorf("summary content missing: %q", payload.Summary)
	}
	final, err := st.GetSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.TokensIn != 500 || final.TokensOut != 60 {
		t.Errorf("compact usage not recorded: %+v", final)
	}
	if saw, _ := st.ListMessages(session.ID); len(saw) != 3 {
		t.Errorf("stored messages = %d, want 3", len(saw))
	}
	if mock.sawTools.Load() {
		t.Error("compaction request should not expose tools")
	}
}

// reasoningOnlyMock returns reasoning but no content, like some models do.
type reasoningOnlyMock struct {
	requests atomic.Int32
}

func (m *reasoningOnlyMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	send(map[string]any{"id": "gen-r1", "choices": []any{
		map[string]any{"delta": map[string]any{"reasoning": "First read the file, then plan."}},
	}})
	send(map[string]any{"id": "gen-r1", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-r1", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 70, "completion_tokens": 30, "total_tokens": 100, "cost": 0.0003,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func TestChatStreamReasoningOnly(t *testing.T) {
	cfg, st, _ := modelsFixture(t)
	projectDir := t.TempDir()
	upstream := httptest.NewServer(&reasoningOnlyMock{})
	defer upstream.Close()

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	project, err := st.CreateProject("Reasoning", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", "PLAN")
	if err != nil {
		t.Fatal(err)
	}

	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	defer app.Close()

	payload, _ := json.Marshal(map[string]any{"session_id": session.ID, "content": "Plan the change"})
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	events := readEvents(t, resp.Body)
	var reasoningOnly bool
	var doneSeen bool
	var contextTokens int
	for _, ev := range events {
		switch ev["type"] {
		case "done":
			doneSeen = true
			reasoningOnly, _ = ev["reasoning_only"].(bool)
		case "usage":
			if n, ok := ev["context_tokens"].(float64); ok {
				contextTokens = int(n)
			}
			if n, ok := ev["completion_tokens"].(float64); ok && n != 30 {
				t.Errorf("completion tokens = %v", n)
			}
		}
	}
	if !doneSeen {
		t.Fatal("no done event")
	}
	if !reasoningOnly {
		t.Error("done.reasoning_only = false, want true")
	}
	if contextTokens != 100 {
		t.Errorf("context_tokens = %d, want 100 (prompt+completion)", contextTokens)
	}

	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Role != "assistant" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if !strings.Contains(msgs[1].Reasoning, "plan") || strings.TrimSpace(msgs[1].Content) != "" {
		t.Errorf("reasoning-only message persisted incorrectly: %+v", msgs[1])
	}
}

func readEvents(t *testing.T, r io.Reader) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var out []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for scanner.Scan() {
		if time.Now().After(deadline) {
			t.Fatal("timed out reading events")
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Turn budget, auto-continue and loop guard

func boolValue(v bool) *bool { return &v }

func countEvents(events []map[string]any, typ string) int {
	n := 0
	for _, ev := range events {
		if ev["type"] == typ {
			n++
		}
	}
	return n
}

func findEvent(events []map[string]any, typ string) map[string]any {
	for _, ev := range events {
		if ev["type"] == typ {
			return ev
		}
	}
	return nil
}

// toolLoopMock always asks for exactly one tool call per round. The args
// callback decides whether rounds look identical (loop) or different (progress).
type toolLoopMock struct {
	requests atomic.Int32
	tool     string
	args     func(n int) string
}

func (m *toolLoopMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(m.requests.Add(1))
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{
		map[string]any{"delta": map[string]any{
			"tool_calls": []any{
				map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function", "function": map[string]any{
					"name": m.tool, "arguments": m.args(n),
				}},
			},
		}},
	}})
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
	}})
	send(map[string]any{"id": fmt.Sprintf("gen-%d", n), "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0.0001,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// planFinalizeMock returns a different read each research round. It answers
// with a plan when the harness sets tool_choice=none.
type planFinalizeMock struct {
	requests                  atomic.Int32
	finalSeen                 atomic.Int32
	sameRange                 bool
	emptyFinalOnce            bool
	invalidFinalOnce          bool
	invalidFinalUnlessNoTools bool
	pseudoResearchOnce        bool
	sawToolChoiceNone         atomic.Bool
	sawNoToolsFinal           atomic.Bool
	sawFirstResult            atomic.Bool
	sawPruned                 atomic.Bool
	finalNote                 atomic.Value
}

func (m *planFinalizeMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(m.requests.Add(1))
	var req struct {
		ToolChoice string `json:"tool_choice"`
		Tools      []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	isFinal := false
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			if strings.HasPrefix(msg.Content, "[pruned]") {
				m.sawPruned.Store(true)
			}
			if strings.Contains(msg.Content, "lines 1-5 of 60") {
				m.sawFirstResult.Store(true)
			}
		}
		if msg.Role == "user" && strings.Contains(msg.Content, "FINAL ANSWER REQUIRED") {
			isFinal = true
			m.finalNote.Store(msg.Content)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if req.ToolChoice == "none" {
		m.sawToolChoiceNone.Store(true)
	}
	noTools := len(req.Tools) == 0
	if noTools && isFinal {
		m.sawNoToolsFinal.Store(true)
	}
	m.respond(w, flusher, send, n, req.ToolChoice, noTools)
}

// finalAnswerMock answers once without requesting tools.
type finalAnswerMock struct{}

func (m *finalAnswerMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	send := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{"content": "All done."}},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{
		map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
	}})
	send(map[string]any{"id": "gen-final", "choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "cost": 0.0001,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func chatBudgetFixture(t *testing.T, mode string, mock http.Handler, tweak func(*config.Config)) (*httptest.Server, *store.Store, store.Session) {
	t.Helper()
	cfg, st, _ := modelsFixture(t)
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"), []byte(strings.Repeat("// line\n", 60)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "file.go"), []byte("package f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(mock)
	t.Cleanup(upstream.Close)

	c := cfg.Get()
	c.Providers.OpenRouter.APIKey = "test-key"
	c.Providers.OpenRouter.BaseURL = upstream.URL
	if tweak != nil {
		tweak(&c)
	}
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}

	project, err := st.CreateProject("Budget", projectDir, "#6c8cff")
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreateSession(project.ID, "New Chat", "mock/model", mode)
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(cfg, st, os.DirFS(".")).Handler())
	t.Cleanup(app.Close)
	return app, st, session
}

func postChatEvents(t *testing.T, app *httptest.Server, body map[string]any) []map[string]any {
	t.Helper()
	payload, _ := json.Marshal(body)
	resp, err := http.Post(app.URL+"/api/chat/stream", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	return readEvents(t, resp.Body)
}

func TestChatStreamPausesAtCheckpoint(t *testing.T) {
	mock := &toolLoopMock{tool: "update_checklist", args: func(n int) string {
		return fmt.Sprintf(`{"action":"add","task_text":"step %d"}`, n)
	}}
	app, st, session := chatBudgetFixture(t, "BUILD", mock, func(c *config.Config) {
		c.Defaults.MaxToolRounds = 2
		c.Defaults.MaxTurnRounds = 4
		c.Defaults.AutoContinue = boolValue(false)
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "work"})

	if got := countEvents(events, "round_start"); got != 2 {
		t.Errorf("round_start = %d, want 2", got)
	}
	if got := countEvents(events, "limit"); got != 0 {
		t.Errorf("limit = %d, want 0", got)
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	cp := findEvent(events, "checkpoint")
	if cp == nil || cp["reason"] != "paused" {
		t.Fatalf("checkpoint = %+v, want reason paused", cp)
	}
	if mock.requests.Load() != 2 {
		t.Errorf("provider requests = %d, want 2", mock.requests.Load())
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("messages = %d, want 3 (user + 2 tool rounds)", len(msgs))
	}
}

func TestChatStreamAutoContinues(t *testing.T) {
	mock := &toolLoopMock{tool: "update_checklist", args: func(n int) string {
		return fmt.Sprintf(`{"action":"add","task_text":"step %d"}`, n)
	}}
	app, st, session := chatBudgetFixture(t, "BUILD", mock, func(c *config.Config) {
		c.Defaults.MaxToolRounds = 2
		c.Defaults.MaxTurnRounds = 4
		c.Defaults.AutoContinue = boolValue(true)
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "work"})

	if got := countEvents(events, "round_start"); got != 4 {
		t.Errorf("round_start = %d, want 4", got)
	}
	if got := countEvents(events, "checkpoint"); got != 1 {
		t.Errorf("checkpoint = %d, want 1", got)
	}
	cp := findEvent(events, "checkpoint")
	if cp == nil || cp["reason"] != "auto" {
		t.Fatalf("checkpoint = %+v, want reason auto", cp)
	}
	limit := findEvent(events, "limit")
	if limit == nil || limit["reason"] != "turn_budget" {
		t.Fatalf("limit = %+v, want reason turn_budget", limit)
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	if got := countEvents(events, "error"); got != 0 {
		t.Errorf("error events = %d, want 0", got)
	}
	if mock.requests.Load() != 4 {
		t.Errorf("provider requests = %d, want 4", mock.requests.Load())
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Errorf("messages = %d, want 5 (user + 4 tool rounds)", len(msgs))
	}
}

func TestChatStreamReportsHistoryPruning(t *testing.T) {
	mock := &toolLoopMock{tool: "read_file", args: func(n int) string {
		return fmt.Sprintf(`{"path":"main.go","start_line":%d,"end_line":%d}`, n, n+20)
	}}
	// A tiny token ceiling forces every older tool round to be stubbed, which
	// is what the inspector's pruning matrix renders.
	app, _, session := chatBudgetFixture(t, "BUILD", mock, func(c *config.Config) {
		c.Defaults.MaxToolRounds = 3
		c.Defaults.MaxTurnRounds = 3
		c.Defaults.AutoContinue = boolValue(false)
		c.Defaults.HistoryPruneMaxTokens = 5
	})

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "work"})

	var sawPruned bool
	for _, ev := range events {
		if ev["type"] != "usage" {
			continue
		}
		rounds, _ := ev["history_rounds"].(float64)
		pruned, _ := ev["pruned_rounds"].(float64)
		chars, _ := ev["pruned_chars"].(float64)
		if rounds <= 0 || pruned <= 0 || chars <= 0 {
			continue
		}
		if pruned > rounds {
			t.Errorf("pruned_rounds = %v > history_rounds = %v", pruned, rounds)
		}
		sawPruned = true
	}
	if !sawPruned {
		t.Fatal("no usage event reported a pruned history")
	}
}

func TestChatStreamLoopGuardStopsRepeatedCall(t *testing.T) {
	mock := &toolLoopMock{tool: "read_file", args: func(int) string {
		return `{"path":"main.go","start_line":1,"end_line":20}`
	}}
	app, st, session := chatBudgetFixture(t, "BUILD", mock, nil)

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "inspect"})

	if mock.requests.Load() != int32(loopAbortRepeats) {
		t.Errorf("provider requests = %d, want %d", mock.requests.Load(), loopAbortRepeats)
	}
	if got := countEvents(events, "round_start"); got != loopAbortRepeats {
		t.Errorf("round_start = %d, want %d", got, loopAbortRepeats)
	}
	limit := findEvent(events, "limit")
	if limit == nil || limit["reason"] != "loop_detected" {
		t.Fatalf("limit = %+v, want reason loop_detected", limit)
	}
	if got := countEvents(events, "error"); got != 0 {
		t.Errorf("error events = %d, want 0 (a loop stop is a pause, not a failure)", got)
	}
	if got := countEvents(events, "done"); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != loopAbortRepeats+1 {
		t.Errorf("messages = %d, want %d (no tool result is lost)", len(msgs), loopAbortRepeats+1)
	}
}

func TestChatStreamToolErrorLimit(t *testing.T) {
	mock := &toolLoopMock{tool: "patch_file", args: func(n int) string {
		return fmt.Sprintf(`{"path":"file.go","search":"missing-%d","replace":"x"}`, n)
	}}
	app, _, session := chatBudgetFixture(t, "BUILD", mock, nil)

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "content": "patch it"})

	if mock.requests.Load() != int32(errAbortStreak) {
		t.Errorf("provider requests = %d, want %d", mock.requests.Load(), errAbortStreak)
	}
	limit := findEvent(events, "limit")
	if limit == nil || limit["reason"] != "tool_error_limit" {
		t.Fatalf("limit = %+v, want reason tool_error_limit", limit)
	}
}

func TestChatStreamContinueFlag(t *testing.T) {
	app, st, session := chatBudgetFixture(t, "PLAN", &finalAnswerMock{}, nil)

	events := postChatEvents(t, app, map[string]any{"session_id": session.ID, "continue": true})
	if got := countEvents(events, "error"); got != 0 {
		t.Errorf("error events = %d, want 0", got)
	}
	msgs, err := st.ListMessages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (synthetic user + assistant)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != continuePrompt {
		t.Errorf("first message = %+v, want the continue prompt", msgs[0])
	}
}

func TestBuildSystemPromptIsStableAndStateFree(t *testing.T) {
	project := store.Project{Name: "demo", Path: "/tmp/demo"}
	session := store.Session{Mode: "PLAN"}
	outline := "FILE internal/volatile.go"

	first := buildSystemPrompt(project, session, 12, 96, 8, 24)
	second := buildSystemPrompt(project, session, 12, 96, 8, 24)
	if first != second {
		t.Fatal("buildSystemPrompt is not deterministic")
	}
	if strings.Contains(first, outline) {
		t.Error("repository outline leaked into the cacheable system prompt")
	}
	if strings.Contains(first, "Current checklist") {
		t.Error("checklist leaked into the cacheable system prompt")
	}
	if !strings.Contains(first, "Response style (strict)") {
		t.Error("system prompt is missing the response-style rules")
	}
}

// TestBuildSystemPromptPLANFinalDetail pins the PLAN deliverable format: the
// plan must arrive with evidence and open questions, not a one-line summary.
func TestBuildSystemPromptPLANFinalDetail(t *testing.T) {
	project := store.Project{Name: "demo", Path: "/tmp/demo"}
	prompt := buildSystemPrompt(project, store.Session{Mode: "PLAN"}, 12, 96, 8, 24)
	for _, want := range []string{
		"## Diagnosis & Approach",
		"## Key Decisions",
		"## Implementation Plan",
		"## Open Questions (Optional)",
		"no raw code blocks",
		"no SQL queries",
		"no verbose inline signatures",
		"Ready to implement — switch to BUILD to apply this plan.",
		"never write a vague summary",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("PLAN system prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"at most 7 one-line items", "BUILD final message", "## Decisions & assumptions", "## Findings"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("PLAN system prompt still contains %q", unwanted)
		}
	}
}

// TestBuildSystemPromptBUILDStaysTerse keeps BUILD output minimal: the detail
// upgrade is PLAN-only.
func TestBuildSystemPromptBUILDStaysTerse(t *testing.T) {
	project := store.Project{Name: "demo", Path: "/tmp/demo"}
	prompt := buildSystemPrompt(project, store.Session{Mode: "BUILD"}, 12, 96, 8, 24)
	if !strings.Contains(prompt, "concise but informative output") {
		t.Error("BUILD system prompt is missing the expanded final-message rule")
	}
	if !strings.Contains(prompt, "what changed with file paths") {
		t.Error("BUILD system prompt missing 'what changed' guidance")
	}
	if !strings.Contains(prompt, "what you verified") {
		t.Error("BUILD system prompt missing 'what you verified' guidance")
	}
	if !strings.Contains(prompt, "next steps for the user") {
		t.Error("BUILD system prompt missing 'next steps' guidance")
	}
	for _, unwanted := range []string{"## Diagnosis & Approach", "## Key Decisions", "## Implementation Plan", "## Open Questions"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("BUILD system prompt must not require the detailed PLAN section %q", unwanted)
		}
	}
}

// TestBuildFinalPlanNoteRequestsDetailedPlan checks the forced final answer asks
// for the same detailed deliverable.
func TestBuildFinalPlanNoteRequestsDetailedPlan(t *testing.T) {
	note := buildFinalPlanNote("", 8, 24, 8, 24, 1, false)
	for _, want := range []string{
		"FINAL ANSWER REQUIRED",
		"## Diagnosis & Approach",
		"## Key Decisions",
		"## Implementation Plan",
		"## Open Questions (Optional)",
		"Ready to implement — switch to BUILD to apply this plan.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("final plan note missing %q", want)
		}
	}
}

func TestBuildStateMessageCarriesVolatileState(t *testing.T) {
	checklist := []store.ChecklistItem{
		{ID: 1, TaskText: "write tests", IsDone: true},
		{ID: 2, TaskText: "run build", IsDone: false},
	}
	state := buildStateMessage(checklist, "FILE internal/demo.go")
	for _, want := range []string{
		"not a new request",
		"[x] #1 write tests",
		"[ ] #2 run build",
		"FILE internal/demo.go",
	} {
		if !strings.Contains(state, want) {
			t.Errorf("state message missing %q:\n%s", want, state)
		}
	}
}
