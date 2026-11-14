package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"forge/internal/llm"
	"forge/internal/tools"
)

// The guard watches the tool calls of a single agent turn and stops pathological
// loops before they consume the whole round budget.
//
// A call is identified by its name, its canonicalized arguments, and a hash of
// its result. Repeating a call does not reset the detector, but any different
// call (for example a successful patch between two failing test runs) moves the
// window forward, so legitimate retry cycles are preserved.
const (
	guardWindow      = 8
	loopWarnRepeats  = 3
	loopAbortRepeats = 4
	errWarnStreak    = 4
	errAbortStreak   = 6
)

type guardAction int

const (
	guardOK guardAction = iota
	guardNudge
	guardAbort
)

type guardVerdict struct {
	Action guardAction
	Note   string // corrective text sent to the model, or the stop reason shown to the user
	Reason string // machine-readable stop reason for the limit event
}

// loopGuard tracks the recent tool-call window and the consecutive error streak.
type loopGuard struct {
	window      []string
	errorStreak int
}

func newLoopGuard() *loopGuard { return &loopGuard{} }

// observe records one executed (or blocked) tool call and reports whether the
// model should be nudged or the turn should be stopped.
func (g *loopGuard) observe(call llm.ToolCall, res tools.Result) guardVerdict {
	fp := callFingerprint(call, res)
	g.window = append(g.window, fp)
	if len(g.window) > guardWindow {
		g.window = g.window[len(g.window)-guardWindow:]
	}
	if res.IsError {
		g.errorStreak++
	} else {
		g.errorStreak = 0
	}

	repeats := 0
	for _, seen := range g.window {
		if seen == fp {
			repeats++
		}
	}

	if repeats >= loopAbortRepeats {
		return guardVerdict{
			Action: guardAbort,
			Reason: "loop_detected",
			Note: fmt.Sprintf(
				"Stopped: %s produced the same result %d times in a short window. The agent is stuck in a loop.",
				call.Function.Name, repeats),
		}
	}
	if g.errorStreak >= errAbortStreak {
		return guardVerdict{
			Action: guardAbort,
			Reason: "tool_error_limit",
			Note: fmt.Sprintf(
				"Stopped: %d tool calls failed in a row. The agent is not recovering from the errors.",
				g.errorStreak),
		}
	}
	if repeats >= loopWarnRepeats {
		return guardVerdict{
			Action: guardNudge,
			Note: fmt.Sprintf(
				"Forge guard: you called %s with the same arguments and got the same result %d times. "+
					"Do not repeat this call. Use the information you already have, change your approach, or finish with a summary.",
				call.Function.Name, repeats),
		}
	}
	if g.errorStreak >= errWarnStreak {
		return guardVerdict{
			Action: guardNudge,
			Note: fmt.Sprintf(
				"Forge guard: %d tool calls in a row have failed. Stop retrying the same approach. "+
					"Re-read the error, change the arguments or the strategy, or finish and report the blocker.",
				g.errorStreak),
		}
	}
	return guardVerdict{Action: guardOK}
}

// callFingerprint identifies a tool call together with the result it produced.
// Two identical calls with different results are treated as distinct, because
// the second one is still producing new information.
func callFingerprint(call llm.ToolCall, res tools.Result) string {
	sum := sha256.Sum256([]byte(res.Content))
	status := "ok"
	if res.IsError {
		status = "error"
	}
	return call.Function.Name + "\x00" +
		canonicalArgs(call.Function.Arguments) + "\x00" +
		hex.EncodeToString(sum[:8]) + "\x00" + status
}

// canonicalArgs normalizes JSON so that equivalent argument objects (different
// key order or whitespace) share a fingerprint. Invalid JSON falls back to the
// raw trimmed string.
func canonicalArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return trimmed
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return string(normalized)
}
