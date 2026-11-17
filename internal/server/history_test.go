package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"forge/internal/llm"
	"forge/internal/store"
)

func toolRow(id int64, name, args, result string) store.Message {
	return toolRowStatus(id, name, args, result, "done")
}

func toolRowStatus(id int64, name, args, result, status string) store.Message {
	call := llm.ToolCall{
		ID:     fmt.Sprintf("call_%d", id),
		Type:   "function",
		Status: status,
		Result: result,
	}
	call.Function = llm.ToolFunction{Name: name, Arguments: args}
	raw, _ := json.Marshal([]llm.ToolCall{call})
	return store.Message{ID: id, Role: "assistant", ToolCalls: string(raw)}
}

func userRow(id int64, content string) store.Message {
	return store.Message{ID: id, Role: "user", Content: content}
}

func finalAssistantRow(id int64, content string) store.Message {
	return store.Message{ID: id, Role: "assistant", Content: content}
}

func TestBuildHistoryKeepsRecentRoundsFull(t *testing.T) {
	rows := []store.Message{userRow(1, "do the task")}
	for i := int64(0); i < 6; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"file%d.go","start_line":1,"end_line":50}`, i),
			"--- content ---\n"+strings.Repeat("line\n", 100)))
	}
	rows = append(rows, finalAssistantRow(99, "done"))

	got, stats := buildHistory(rows, "system", historyOptions{KeepRecent: 2, ChunkRounds: 1})

	if stats.PrunedRounds != 4 {
		t.Errorf("PrunedRounds = %d, want 4", stats.PrunedRounds)
	}
	if stats.Rounds != 6 {
		t.Errorf("Rounds = %d, want 6", stats.Rounds)
	}
	if stats.PrunedChars <= 0 {
		t.Error("PrunedChars should be positive")
	}
	// Nothing may be removed or reordered: system + user + 6 rounds + final.
	if len(got) != 1+1+6+6+1 {
		t.Fatalf("message count = %d, want 15", len(got))
	}
	if got[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", got[0].Role)
	}
	if got[1].Role != "user" || got[1].Content != "do the task" {
		t.Errorf("user message was not preserved: %+v", got[1])
	}

	var stubbed, full int
	var seenTool []string
	for _, m := range got {
		if m.Role == "tool" {
			seenTool = append(seenTool, m.ToolCallID)
			switch {
			case strings.HasPrefix(m.Content, stubPrefix):
				stubbed++
			case strings.Contains(m.Content, "--- content ---"):
				full++
			}
		}
	}
	if stubbed != 4 || full != 2 {
		t.Errorf("stubbed/full tool messages = %d/%d, want 4/2", stubbed, full)
	}
	wantIDs := []string{"call_10", "call_11", "call_12", "call_13", "call_14", "call_15"}
	if !reflect.DeepEqual(seenTool, wantIDs) {
		t.Errorf("tool messages were reordered: got %v, want %v", seenTool, wantIDs)
	}
	if got[len(got)-1].Role != "assistant" || got[len(got)-1].Content != "done" {
		t.Errorf("final assistant message was not preserved: %+v", got[len(got)-1])
	}
	assertNoOrphanToolMessages(t, got)
}

func TestBuildHistoryClipsLargeResults(t *testing.T) {
	body := strings.Repeat("A", 8000) + "\nMIDDLE\n" + strings.Repeat("Z", 8000)
	rows := []store.Message{
		userRow(1, "read"),
		toolRow(2, "bash", `{"command":"go test ./..."}`, body),
	}
	got, _ := buildHistory(rows, "system", historyOptions{KeepRecent: 4, MaxResultChars: 1000, ChunkRounds: 1})

	var result string
	for _, m := range got {
		if m.Role == "tool" {
			result = m.Content
		}
	}
	if result == "" {
		t.Fatal("tool result missing")
	}
	if n := utf8.RuneCountInString(result); n > 1100 {
		t.Errorf("clipped result is %d chars, want near 1000", n)
	}
	if !strings.HasPrefix(result, "AAAA") {
		t.Error("clipped result lost its head")
	}
	if !strings.HasSuffix(result, "ZZZZ") {
		t.Error("clipped result lost its tail")
	}
	if !strings.Contains(result, "characters pruned") {
		t.Error("clipped result is missing the pruning marker")
	}
}

func TestBuildHistoryDropsWriteFileContent(t *testing.T) {
	content := strings.Repeat("package main\n", 3000)
	args, _ := json.Marshal(map[string]any{
		"path":    "large.go",
		"content": content,
	})
	rows := []store.Message{
		userRow(1, "write"),
		toolRow(2, "write_file", string(args), "Wrote large.go (36 KB, 3000 lines)."),
	}
	got, _ := buildHistory(rows, "system", historyOptions{KeepRecent: 4, MaxArgChars: 1000, ChunkRounds: 1})

	var sent string
	for _, m := range got {
		for _, c := range m.ToolCalls {
			if c.Function.Name == "write_file" {
				sent = c.Function.Arguments
			}
		}
	}
	if sent == "" {
		t.Fatal("write_file tool call missing")
	}
	if strings.Contains(sent, "package main") {
		t.Error("write_file content was re-sent in the history")
	}
	if utf8.RuneCountInString(sent) > 1000 {
		t.Errorf("compacted arguments are %d chars, want <= 1000", utf8.RuneCountInString(sent))
	}
	assertPrunedArgs(t, sent)
}

func TestBuildHistoryStubsOldWriteFileContent(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"path": "small.go", "content": "hello forge"})
	rows := []store.Message{
		userRow(1, "write"),
		toolRow(2, "write_file", string(args), "Wrote small.go."),
		toolRow(3, "read_file", `{"path":"small.go","start_line":1,"end_line":1}`, "1 | hello forge"),
	}
	got, stats := buildHistory(rows, "system", historyOptions{KeepRecent: 1, MaxArgChars: 10000, ChunkRounds: 1})
	if stats.PrunedRounds != 1 {
		t.Fatalf("PrunedRounds = %d, want 1", stats.PrunedRounds)
	}
	var sent string
	for _, m := range got {
		for _, c := range m.ToolCalls {
			if c.Function.Name == "write_file" {
				sent = c.Function.Arguments
			}
		}
	}
	if strings.Contains(sent, "hello forge") {
		t.Errorf("old write_file content survived even under the size cap: %s", sent)
	}
	assertPrunedArgs(t, sent)
}

func assertPrunedArgs(t *testing.T, sent string) {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(sent), &parsed); err != nil {
		t.Fatalf("compacted arguments are not valid JSON: %v (%q)", err, sent)
	}
	if parsed["_pruned"] != true {
		t.Error("compacted arguments are missing _pruned")
	}
	if parsed["path"] != "large.go" && parsed["path"] != "small.go" {
		t.Errorf("compacted arguments lost the path: %v", parsed)
	}
}

func TestBuildHistoryPreservesOldErrors(t *testing.T) {
	longErr := strings.Repeat("x", 2000)
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 3; i++ {
		rows = append(rows, toolRowStatus(10+i, "read_file",
			`{"path":"missing.go","start_line":1,"end_line":10}`, longErr, "error"))
	}
	rows = append(rows, toolRowStatus(20, "bash", `{"command":"go test ./..."}`, "boom", "error"))
	rows = append(rows, finalAssistantRow(99, "done"))

	got, _ := buildHistory(rows, "system", historyOptions{KeepRecent: 1, ChunkRounds: 1})
	var stubs int
	for _, m := range got {
		if m.Role == "tool" && strings.HasPrefix(m.Content, stubPrefix) {
			stubs++
			if !strings.Contains(m.Content, "ERROR:") {
				t.Errorf("stub lost error status: %q", m.Content)
			}
			if !strings.Contains(m.Content, "read_file missing.go") {
				t.Errorf("stub lost the failing call: %q", m.Content)
			}
		}
	}
	if stubs != 3 {
		t.Errorf("stubbed errors = %d, want 3", stubs)
	}
}

func TestBuildHistoryDeterministic(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 8; i++ {
		rows = append(rows, toolRow(10+i, "grep_search", `{"pattern":"func main","path":"."}`, "main.go:10: func main() {"))
	}
	rows = append(rows, finalAssistantRow(99, "done"))
	opts := historyOptions{KeepRecent: 2, MaxResultChars: 200, MaxArgChars: 200, ChunkRounds: 4}

	first, _ := buildHistory(rows, "system", opts)
	second, _ := buildHistory(rows, "system", opts)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Error("buildHistory is not deterministic; prompt caching would break")
	}
}

func TestBuildHistoryPrefixStableWithinChunk(t *testing.T) {
	base := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 8; i++ {
		base = append(base, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
			"1 | content\n"))
	}
	opts := historyOptions{KeepRecent: 4, ChunkRounds: 4}

	first, _ := buildHistory(base, "system", opts)
	second, _ := buildHistory(append(append([]store.Message{}, base...),
		toolRow(30, "read_file", `{"path":"f8.go","start_line":1,"end_line":10}`, "1 | content\n")), "system", opts)

	if len(first) >= len(second) {
		t.Fatalf("second history should be longer: %d vs %d", len(first), len(second))
	}
	if !reflect.DeepEqual(first, second[:len(first)]) {
		t.Error("history within a pruning chunk is not an append-only prefix; prompt cache would break")
	}
}

func TestBuildHistoryTokenBudgetKeepsNewestRounds(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 10; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
			strings.Repeat("x", 2000)))
	}
	// 10 rounds × 2000 chars ≈ 5000 tokens, so a 3000-token budget must stub the
	// oldest rounds and keep the newest ones verbatim.
	got, stats := buildHistory(rows, "system", historyOptions{
		TokenBudget: 3000,
		ChunkRounds: 4,
	})
	if stats.PrunedRounds == 0 {
		t.Fatal("token budget should have stubbed the oldest rounds")
	}
	if stats.PrunedRounds%4 != 0 {
		t.Errorf("PrunedRounds = %d, want a multiple of the chunk size 4", stats.PrunedRounds)
	}
	if stats.Chars/4 > 3000 {
		t.Errorf("estimated tokens = %d, want <= 3000", stats.Chars/4)
	}
	full := len(rows) - 1 - stats.PrunedRounds
	var kept int
	for _, m := range got {
		if m.Role == "tool" && !strings.HasPrefix(m.Content, stubPrefix) {
			kept++
		}
	}
	if kept != full {
		t.Errorf("kept %d full tool results, want %d", kept, full)
	}
	assertNoOrphanToolMessages(t, got)
}

func TestBuildHistoryNoTokenBudgetKeepsLegacyBehavior(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 6; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
			strings.Repeat("x", 100)))
	}
	got, stats := buildHistory(rows, "system", historyOptions{KeepRecent: 2, ChunkRounds: 1})
	if stats.PrunedRounds != 4 {
		t.Errorf("PrunedRounds = %d, want 4 from the round-based fallback", stats.PrunedRounds)
	}
	assertNoOrphanToolMessages(t, got)
}

func TestBuildHistoryBudgetMinRecentFloor(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 5; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
			strings.Repeat("x", 40000)))
	}
	// A budget far too small for the newest rounds must still not prune the
	// MinRecent floor: PLAN needs its current research turn to survive.
	_, stats := buildHistory(rows, "system", historyOptions{
		TokenBudget: 1000,
		ChunkRounds: 1,
		MinRecent:   2,
	})
	if stats.PrunedRounds != 3 {
		t.Errorf("PrunedRounds = %d, want 3 (5 rounds minus the 2-round floor)", stats.PrunedRounds)
	}
}

func TestBuildHistoryChunkDelaysPruning(t *testing.T) {
	rows := []store.Message{userRow(1, "go")}
	for i := int64(0); i < 8; i++ {
		rows = append(rows, toolRow(10+i, "read_file",
			fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i), "1 | content\n"))
	}
	opts := historyOptions{KeepRecent: 4, ChunkRounds: 4}

	seven, statsSeven := buildHistory(rows[:8], "system", opts)
	if statsSeven.PrunedRounds != 0 {
		t.Errorf("7 rounds: PrunedRounds = %d, want 0 (chunk not reached)", statsSeven.PrunedRounds)
	}
	for _, m := range seven {
		if m.Role == "tool" && strings.HasPrefix(m.Content, stubPrefix) {
			t.Error("7 rounds: no tool message should be stubbed yet")
		}
	}

	_, statsEight := buildHistory(rows, "system", opts)
	if statsEight.PrunedRounds != 4 {
		t.Errorf("8 rounds: PrunedRounds = %d, want 4 (whole chunk)", statsEight.PrunedRounds)
	}
}

func assertNoOrphanToolMessages(t *testing.T, msgs []llm.Message) {
	t.Helper()
	known := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			known[c.ID] = true
		}
		if m.Role == "tool" && !known[m.ToolCallID] {
			t.Errorf("tool message %q has no matching tool call", m.ToolCallID)
		}
	}
}

func TestBuildHistoryPrefixProperty(t *testing.T) {
	for keep := 1; keep <= 4; keep++ {
		for chunk := 1; chunk <= 4; chunk++ {
			opts := historyOptions{KeepRecent: keep, ChunkRounds: chunk}
			var prev []llm.Message
			for n := 0; n <= 16; n++ {
				rows := []store.Message{userRow(1, "go")}
				for i := 0; i < n; i++ {
					rows = append(rows, toolRow(int64(10+i), "read_file",
						fmt.Sprintf(`{"path":"f%d.go","start_line":1,"end_line":10}`, i),
						strings.Repeat("x", 1000)))
				}
				cur, _ := buildHistory(rows, "system", opts)
				if prev != nil && pruneBoundary(n-1, keep, chunk) == pruneBoundary(n, keep, chunk) {
					if len(prev) > len(cur) || !reflect.DeepEqual(prev, cur[:len(prev)]) {
						t.Fatalf("keep=%d chunk=%d rounds=%d: request is not append-only inside a chunk",
							keep, chunk, n)
					}
				}
				prev = cur
			}
		}
	}
}
