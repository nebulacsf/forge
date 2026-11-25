package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"forge/internal/llm"
	"forge/internal/tools"
)

// researchLedger records what the agent already inspected during one PLAN turn.
// It is rendered into the trailing ephemeral state message so the model does not
// need to re-read ranges that are already present in the conversation. This is
// intentionally per-turn: a new user message starts a fresh research budget.
type researchLedger struct {
	reads   map[string][]readRange
	greps   map[string]int
	symbols map[string]int
	rounds  int
	calls   int
}

type readRange struct {
	Start int
	End   int
}

func newResearchLedger() *researchLedger {
	return &researchLedger{
		reads:   make(map[string][]readRange),
		greps:   make(map[string]int),
		symbols: make(map[string]int),
	}
}

// observe records a successful tool call. Failed calls are skipped so the model
// can retry with corrected arguments without the ledger claiming it already
// inspected something it never saw.
func (l *researchLedger) observe(call llm.ToolCall, res tools.Result) {
	if l == nil || res.IsError {
		return
	}
	switch call.Function.Name {
	case "read_file":
		var a struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil {
			return
		}
		l.addRead(cleanLedgerPath(a.Path), a.StartLine, a.EndLine)
	case "grep_search":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Glob    string `json:"glob"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil {
			return
		}
		pattern := strings.TrimSpace(a.Pattern)
		if pattern == "" {
			return
		}
		key := "/" + clipRunes(pattern, 60) + "/"
		if a.Path != "" {
			key += " in " + cleanLedgerPath(a.Path)
		}
		if a.Glob != "" {
			key += " glob " + a.Glob
		}
		l.greps[key]++
	case "symbol_lookup":
		var a struct {
			Query string `json:"query"`
			Kind  string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil {
			return
		}
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return
		}
		key := clipRunes(query, 60)
		if a.Kind != "" {
			key += " [" + a.Kind + "]"
		}
		l.symbols[key]++
	}
}

func (l *researchLedger) addRead(path string, start, end int) {
	if path == "" || start <= 0 {
		return
	}
	if end < start {
		end = start
	}
	ranges := append(l.reads[path], readRange{Start: start, End: end})
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})
	merged := make([]readRange, 0, len(ranges))
	for _, r := range ranges {
		if len(merged) == 0 {
			merged = append(merged, r)
			continue
		}
		last := &merged[len(merged)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	// A single turn should never need dozens of distinct windows in one file.
	// Keep the most recent windows when the list grows; the budget will end the
	// turn shortly after this point anyway.
	if len(merged) > 10 {
		merged = merged[len(merged)-10:]
	}
	l.reads[path] = merged
}

// render produces a compact, token-bounded ledger. It always includes the
// remaining budget so the model can see when it must stop researching; a limit
// configured as 0 is reported as absent instead of as a share of zero.
func (l *researchLedger) render(planRounds, planToolCalls int) string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Research budget: ")
	b.WriteString(formatLedgerUse(l.rounds, planRounds, "tool round"))
	b.WriteString(", ")
	b.WriteString(formatLedgerUse(l.calls, planToolCalls, "tool call"))
	b.WriteString(" used.")
	if remaining := planRounds - l.rounds; planRounds > 0 && remaining <= 2 {
		b.WriteString(" Tools will be disabled at the budget; start writing the final plan now.")
	}
	if len(l.reads) == 0 && len(l.greps) == 0 && len(l.symbols) == 0 {
		return clipRunes(strings.TrimSpace(b.String()), 900)
	}

	b.WriteString("\nResearch ledger (this turn; do not repeat these calls):")
	paths := make([]string, 0, len(l.reads))
	for path := range l.reads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for i, path := range paths {
		if i >= 6 {
			fmt.Fprintf(&b, "\n- …and %d more file(s) already read", len(paths)-i)
			break
		}
		parts := make([]string, 0, len(l.reads[path]))
		for j, r := range l.reads[path] {
			if j >= 4 {
				parts = append(parts, "…")
				break
			}
			parts = append(parts, fmt.Sprintf("L%d-%d", r.Start, r.End))
		}
		fmt.Fprintf(&b, "\n- read %s %s", path, strings.Join(parts, ", "))
	}

	symbols := make([]string, 0, len(l.symbols))
	for key := range l.symbols {
		symbols = append(symbols, key)
	}
	sort.Strings(symbols)
	for i, key := range symbols {
		if i >= 6 {
			fmt.Fprintf(&b, "\n- …and %d more symbol lookup(s) already run", len(symbols)-i)
			break
		}
		if n := l.symbols[key]; n > 1 {
			fmt.Fprintf(&b, "\n- symbol %s (x%d)", key, n)
		} else {
			fmt.Fprintf(&b, "\n- symbol %s", key)
		}
	}

	searches := make([]string, 0, len(l.greps))
	for key := range l.greps {
		searches = append(searches, key)
	}
	sort.Strings(searches)
	for i, key := range searches {
		if i >= 6 {
			fmt.Fprintf(&b, "\n- …and %d more search(es) already run", len(searches)-i)
			break
		}
		if n := l.greps[key]; n > 1 {
			fmt.Fprintf(&b, "\n- grep %s (x%d)", key, n)
		} else {
			fmt.Fprintf(&b, "\n- grep %s", key)
		}
	}
	return clipRunes(strings.TrimSpace(b.String()), 900)
}

// formatLedgerUse renders "N/limit unit(s)" for a bounded budget and "N unit(s)"
// when the limit is 0, which is how the ledger reports an absent ceiling.
func formatLedgerUse(used, limit int, unit string) string {
	label := unit
	if used != 1 {
		label = unit + "s"
	}
	if limit <= 0 {
		return fmt.Sprintf("%d %s (no limit)", used, label)
	}
	return fmt.Sprintf("%d/%d %s", used, limit, label)
}

func cleanLedgerPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	path = strings.TrimPrefix(path, "./")
	if path == "" {
		return "."
	}
	return path
}
