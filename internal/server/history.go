package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"forge/internal/llm"
	"forge/internal/store"
)

// History pruning keeps the most recent tool rounds verbatim and stubs older
// rounds in place. Nothing is removed or reordered, so the message prefix stays
// appending-only for the provider's prompt cache. The full results remain in
// SQLite and in the UI; only the provider request is trimmed.
//
// Volatile state (checklist, repository outline) is intentionally NOT part of
// the system prompt: it lives in a trailing ephemeral message so the stable
// prefix can be cached across rounds.
const (
	defaultHistoryKeepRecent = 4
	defaultHistoryMaxResult  = 4000
	defaultHistoryMaxArg     = 4000
	defaultHistoryChunk      = 4

	// historyCharsPerToken is the cheap estimation used everywhere in the
	// harness (buildHistory stats, the UI meter). It deliberately over-estimates
	// for prose and under-estimates for dense code, so the token budget keeps a
	// safety margin before the provider's real tokenizer is involved.
	historyCharsPerToken = 4

	stubPrefix      = "[pruned] "
	oldContentLimit = 600
)

type historyOptions struct {
	KeepRecent     int
	MaxResultChars int // 0 disables the cap
	MaxArgChars    int // 0 disables the cap
	ChunkRounds    int // 1 prunes every round; >1 prunes in stable chunks
	// TokenBudget is the estimated token ceiling for the whole request
	// (system prompt + tool definitions + conversation). When positive it
	// drives pruning and the round-based knobs are ignored. 0 disables the
	// budget and falls back to KeepRecent/ChunkRounds.
	TokenBudget int
	// ReservedChars is request overhead that is neither the system prompt nor
	// the conversation (tool definitions, for example). It is subtracted from
	// the token budget before any tool round is kept verbatim.
	ReservedChars int
	// MinRecent is the number of newest tool rounds kept verbatim even when a
	// single oversized round blows past TokenBudget. PLAN uses it so the current
	// research turn is never pruned; 0 lets the budget decide alone.
	MinRecent int
}

type historyStats struct {
	Messages     int
	Chars        int
	Rounds       int // tool rounds found in the persisted history
	PrunedRounds int
	PrunedChars  int
}

func normalizeHistoryOptions(o historyOptions) historyOptions {
	// A negative value means "not configured" and falls back to the shipped
	// default. An explicit 0 is honored: KeepRecent 0 keeps every round verbatim
	// and the Max* 0 caps disable the clipping, as documented in the settings.
	if o.KeepRecent < 0 {
		o.KeepRecent = defaultHistoryKeepRecent
	}
	if o.MaxResultChars < 0 {
		o.MaxResultChars = defaultHistoryMaxResult
	}
	if o.MaxArgChars < 0 {
		o.MaxArgChars = defaultHistoryMaxArg
	}
	// ChunkRounds is not a ceiling but the granularity of the pruning boundary,
	// so 0 selects the default chunk instead of meaning "no limit".
	if o.ChunkRounds <= 0 {
		o.ChunkRounds = defaultHistoryChunk
	}
	return o
}

// pruneBoundary returns how many of the oldest tool rounds must be stubbed.
// It moves in chunks so most consecutive requests keep an identical prefix.
func pruneBoundary(toolRounds, keep, chunk int) int {
	if toolRounds <= keep {
		return 0
	}
	if chunk < 1 {
		chunk = 1
	}
	excess := toolRounds - keep
	return (excess / chunk) * chunk
}

// budgetBoundary returns how many of the oldest tool rounds must be stubbed so
// the request fits in opts.TokenBudget. Rounds are kept verbatim from the newest
// backwards until the ceiling is reached. The boundary advances in chunks of
// opts.ChunkRounds so consecutive requests keep an identical prefix, which is
// what the provider's prompt cache needs to hit.
func budgetBoundary(rows []store.Message, system string, opts historyOptions) int {
	limit := opts.TokenBudget*historyCharsPerToken -
		utf8.RuneCountInString(system) - opts.ReservedChars

	rounds := make([]int, 0, len(rows))
	base := 0
	for _, m := range rows {
		calls := parseToolCalls(m)
		if m.Role == "assistant" && len(calls) > 0 {
			rounds = append(rounds, messageFullChars(m))
			continue
		}
		base += messageFullChars(m)
	}
	limit -= base
	if limit <= 0 || len(rounds) == 0 {
		return len(rounds)
	}

	used := 0
	keep := 0
	for i := len(rounds) - 1; i >= 0; i-- {
		if used+rounds[i] > limit {
			break
		}
		used += rounds[i]
		keep++
	}
	boundary := len(rounds) - keep

	// Round the boundary UP to the next chunk: stubbing a little more than
	// strictly needed keeps the request inside the ceiling and freezes the
	// boundary for the next ChunkRounds rounds, so the prompt prefix stays
	// cacheable instead of shifting on every request.
	chunk := opts.ChunkRounds
	if chunk < 1 {
		chunk = 1
	}
	boundary = ((boundary + chunk - 1) / chunk) * chunk
	if boundary > len(rounds) {
		boundary = len(rounds)
	}
	return boundary
}

// renderToolRound renders one assistant tool round plus its tool results.
func renderToolRound(m store.Message, calls []llm.ToolCall, stubbed bool, opts historyOptions) []llm.Message {
	content := m.Content
	if stubbed && utf8.RuneCountInString(content) > oldContentLimit {
		content = clipRunes(content, oldContentLimit)
	}
	wire := make([]llm.ToolCall, 0, len(calls))
	for _, c := range calls {
		wire = append(wire, llm.ToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: llm.ToolFunction{
				Name:      c.Function.Name,
				Arguments: compactToolArguments(c.Function.Arguments, opts.MaxArgChars, stubbed),
			},
		})
	}
	if strings.TrimSpace(content) == "" && len(wire) == 0 {
		return nil
	}
	out := []llm.Message{{Role: "assistant", Content: content, ToolCalls: wire}}
	for _, c := range calls {
		result := c.Result
		if stubbed {
			result = stubPrefix + digestToolCall(c)
		} else {
			if c.Status == "error" {
				result = "ERROR: " + result
			}
			if strings.TrimSpace(result) == "" {
				result = "(no result recorded)"
			}
			result = clipToolResult(result, opts.MaxResultChars)
		}
		out = append(out, llm.Message{
			Role:       "tool",
			ToolCallID: c.ID,
			Name:       c.Function.Name,
			Content:    result,
		})
	}
	return out
}

// toolSpecsChars estimates the characters the tool definitions occupy on the
// wire. They travel with every request but are not part of the conversation, so
// the token budget must subtract them before deciding how much history fits.
func toolSpecsChars(specs []llm.Tool) int {
	total := 0
	for _, spec := range specs {
		total += utf8.RuneCountInString(spec.Function.Name)
		total += utf8.RuneCountInString(spec.Function.Description)
		if raw, err := json.Marshal(spec.Function.Parameters); err == nil {
			total += len(raw)
		}
	}
	return total
}

// messagesChars estimates the characters a rendered history occupies, which is
// the number the token budget is compared against (4 chars ≈ 1 token).
func messagesChars(msgs []llm.Message) int {
	total := 0
	for _, msg := range msgs {
		total += utf8.RuneCountInString(msg.Content)
		for _, c := range msg.ToolCalls {
			total += utf8.RuneCountInString(c.Function.Arguments)
		}
	}
	return total
}

// buildHistory reconstructs OpenAI-compatible messages from persisted rows,
// stubbing old tool output so each round sends a bounded context.
func buildHistory(rows []store.Message, system string, opts historyOptions) ([]llm.Message, historyStats) {
	opts = normalizeHistoryOptions(opts)

	fullChars := utf8.RuneCountInString(system)
	toolRounds := 0
	for _, m := range rows {
		fullChars += messageFullChars(m)
		if m.Role == "assistant" && len(parseToolCalls(m)) > 0 {
			toolRounds++
		}
	}
	var collapseBefore int
	switch {
	case opts.TokenBudget > 0:
		// Budget-driven pruning: keep as many recent rounds verbatim as the
		// token ceiling allows, so large-context models actually get used.
		collapseBefore = budgetBoundary(rows, system, opts)
	case opts.KeepRecent <= 0:
		// With the budget off and no round window there is nothing to trim:
		// every round travels verbatim, which is what history_keep_recent_rounds
		// = 0 asks for.
		collapseBefore = 0
	default:
		collapseBefore = pruneBoundary(toolRounds, opts.KeepRecent, opts.ChunkRounds)
	}
	// PLAN keeps its whole research turn verbatim even if one huge read blows
	// past the budget by itself; the findings must survive to the final plan.
	if floor := opts.MinRecent; floor > 0 && collapseBefore > toolRounds-floor {
		collapseBefore = toolRounds - floor
		if collapseBefore < 0 {
			collapseBefore = 0
		}
	}

	out := make([]llm.Message, 0, len(rows)+1)
	out = append(out, llm.Message{Role: "system", Content: system})
	stats := historyStats{}
	seenRounds := 0

	for _, m := range rows {
		calls := parseToolCalls(m)
		isToolRound := m.Role == "assistant" && len(calls) > 0
		stubbed := false
		if isToolRound {
			stubbed = seenRounds < collapseBefore
			seenRounds++
		}

		switch m.Role {
		case "user":
			out = append(out, llm.Message{Role: "user", Content: m.Content})
		case "assistant":
			if !isToolRound {
				if strings.TrimSpace(m.Content) == "" {
					continue
				}
				out = append(out, llm.Message{Role: "assistant", Content: m.Content})
				continue
			}
			out = append(out, renderToolRound(m, calls, stubbed, opts)...)
			if stubbed {
				stats.PrunedRounds++
			}
		}
	}

	sentChars := messagesChars(out)
	if fullChars < sentChars {
		fullChars = sentChars
	}
	stats.Messages = len(out)
	stats.Chars = sentChars
	stats.Rounds = toolRounds
	stats.PrunedChars = fullChars - sentChars
	return out, stats
}

// parseToolCalls decodes a persisted tool_calls column and fills in stable IDs
// and types for rows written by older versions.
func parseToolCalls(m store.Message) []llm.ToolCall {
	if strings.TrimSpace(m.ToolCalls) == "" {
		return nil
	}
	var calls []llm.ToolCall
	if err := json.Unmarshal([]byte(m.ToolCalls), &calls); err != nil {
		return nil
	}
	named := calls[:0]
	for i := range calls {
		if calls[i].Function.Name == "" {
			continue
		}
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", m.ID, i)
		}
		if calls[i].Type == "" {
			calls[i].Type = "function"
		}
		named = append(named, calls[i])
	}
	return named
}

func messageFullChars(m store.Message) int {
	n := utf8.RuneCountInString(m.Content)
	for _, c := range parseToolCalls(m) {
		n += utf8.RuneCountInString(c.Function.Arguments)
		n += utf8.RuneCountInString(c.Result)
	}
	return n
}

// clipToolResult keeps the first 60% and the last 40% of an oversized result,
// which preserves headers and trailing error/exit lines.
func clipToolResult(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max * 3 / 5
	tail := max - head
	removed := len(r) - head - tail
	return string(r[:head]) +
		fmt.Sprintf("\n…[%d characters pruned]…\n", removed) +
		string(r[len(r)-tail:])
}

// compactToolArguments replaces oversized tool arguments with a small, valid
// JSON object. When force is true (older rounds), it always drops payload-heavy
// fields such as write_file content, even if the original happened to be small.
func compactToolArguments(raw string, limit int, force bool) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	if !force && (limit <= 0 || utf8.RuneCountInString(raw) <= limit) {
		return raw
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return `{"_pruned":true}`
	}
	kept := make(map[string]any, len(parsed))
	dropped := false
	for k, v := range parsed {
		if !compactArgKeys[k] {
			dropped = true
			continue
		}
		switch s := v.(type) {
		case string:
			switch k {
			case "command":
				kept[k] = clipRunes(collapseSpace(s), 200)
			case "task_text":
				kept[k] = clipRunes(s, 120)
			default:
				kept[k] = clipRunes(s, 300)
			}
		default:
			kept[k] = v
		}
	}
	if dropped {
		kept["_pruned"] = true
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return `{"_pruned":true}`
	}
	if !force && limit > 0 && utf8.RuneCountInString(string(encoded)) > limit {
		return `{"_pruned":true}`
	}
	return string(encoded)
}

var compactArgKeys = map[string]bool{
	"path":           true,
	"pattern":        true,
	"glob":           true,
	"command":        true,
	"action":         true,
	"id":             true,
	"task_text":      true,
	"start_line":     true,
	"end_line":       true,
	"depth":          true,
	"staged":         true,
	"stat":           true,
	"case_sensitive": true,
	"max_results":    true,
	"replace_all":    true,
}

func digestToolCall(c llm.ToolCall) string {
	args := decodeToolArguments(c.Function.Arguments)
	line := c.Function.Name
	if target := digestTarget(c.Function.Name, args); target != "" {
		line += " " + target
	}
	if outcome := digestOutcome(c.Function.Name, c.Result, c.Status); outcome != "" {
		line += " → " + outcome
	}
	return clipRunes(line, oldContentLimit)
}

func digestTarget(name string, args map[string]any) string {
	path := argString(args, "path")
	switch name {
	case "read_file":
		if path == "" {
			path = "."
		}
		start, hasStart := argInt(args, "start_line")
		end, hasEnd := argInt(args, "end_line")
		switch {
		case hasStart && hasEnd:
			return fmt.Sprintf("%s L%d-%d", path, start, end)
		case hasStart:
			return fmt.Sprintf("%s L%d", path, start)
		default:
			return path
		}
	case "grep_search":
		target := "/" + clipRunes(argString(args, "pattern"), 60) + "/"
		if path != "" {
			target += " in " + path
		}
		if glob := argString(args, "glob"); glob != "" {
			target += " glob " + clipRunes(glob, 40)
		}
		return target
	case "symbol_lookup":
		target := clipRunes(argString(args, "query"), 80)
		if kind := argString(args, "kind"); kind != "" {
			target += " [" + kind + "]"
		}
		return target
	case "list_directory":
		if path == "" {
			return "."
		}
		return path
	case "git_diff":
		flags := ""
		if argBool(args, "stat") {
			flags += " --stat"
		}
		if argBool(args, "staged") {
			flags += " --cached"
		}
		if path != "" {
			return path + flags
		}
		return strings.TrimSpace(flags)
	case "update_checklist":
		action := argString(args, "action")
		if id, ok := argInt(args, "id"); ok {
			return fmt.Sprintf("%s #%d", action, id)
		}
		return action
	case "patch_file", "write_file":
		return path
	case "bash":
		return "$ " + clipRunes(collapseSpace(argString(args, "command")), 80)
	default:
		return ""
	}
}

func digestOutcome(name, result, status string) string {
	result = strings.TrimSpace(result)
	if status == "error" || status == "blocked" {
		if result == "" {
			return "ERROR"
		}
		return "ERROR: " + clipRunes(firstNonEmptyLine(result), oldContentLimit)
	}
	if result == "" {
		return "ok"
	}
	switch name {
	case "list_directory":
		entries := -1 // the first line is the directory label
		for _, line := range strings.Split(result, "\n") {
			if strings.TrimSpace(line) != "" {
				entries++
			}
		}
		if entries < 0 {
			entries = 0
		}
		return fmt.Sprintf("%d entries", entries)
	case "bash":
		return clipRunes(tailLines(result, 2, " · "), oldContentLimit)
	default:
		return clipRunes(firstNonEmptyLine(result), 120)
	}
}

func decodeToolArguments(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil
	}
	return args
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func argInt(args map[string]any, key string) (int, bool) {
	if args == nil {
		return 0, false
	}
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func argBool(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	v, _ := args[key].(bool)
	return v
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func tailLines(s string, n int, sep string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	tail := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(tail) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			tail = append(tail, line)
		}
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	return strings.Join(tail, sep)
}

func clipRunes(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
