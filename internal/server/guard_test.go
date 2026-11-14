package server

import (
	"fmt"
	"testing"

	"forge/internal/llm"
	"forge/internal/tools"
)

func guardCall(name, args string) llm.ToolCall {
	return llm.ToolCall{Type: "function", Function: llm.ToolFunction{Name: name, Arguments: args}}
}

func TestLoopGuardAbortsRepeatedCall(t *testing.T) {
	g := newLoopGuard()
	call := guardCall("read_file", `{"path":"a.go","start_line":1,"end_line":20}`)
	res := tools.Result{Content: "package a"}

	for i := 1; i < loopWarnRepeats; i++ {
		if v := g.observe(call, res); v.Action != guardOK {
			t.Fatalf("call %d = %v, want OK", i, v.Action)
		}
	}
	if v := g.observe(call, res); v.Action != guardNudge {
		t.Fatalf("call %d = %v, want nudge", loopWarnRepeats, v.Action)
	}
	for i := loopWarnRepeats + 1; i < loopAbortRepeats; i++ {
		if v := g.observe(call, res); v.Action != guardNudge {
			t.Fatalf("call %d = %v, want nudge", i, v.Action)
		}
	}
	v := g.observe(call, res)
	if v.Action != guardAbort || v.Reason != "loop_detected" {
		t.Fatalf("call %d = %+v, want abort loop_detected", loopAbortRepeats, v)
	}
}

func TestLoopGuardCanonicalizesArguments(t *testing.T) {
	g := newLoopGuard()
	a := guardCall("read_file", `{"path":"a.go","start_line":1,"end_line":20}`)
	b := guardCall("read_file", `{ "end_line": 20, "start_line": 1, "path": "a.go" }`)
	res := tools.Result{Content: "same"}

	g.observe(a, res)
	g.observe(b, res)
	if v := g.observe(a, res); v.Action != guardNudge {
		t.Fatalf("reordered args = %v, want nudge (same fingerprint)", v.Action)
	}
}

func TestLoopGuardAllowsDifferentResults(t *testing.T) {
	g := newLoopGuard()
	call := guardCall("bash", `{"command":"go test ./..."}`)
	for i := 0; i < 8; i++ {
		if v := g.observe(call, tools.Result{Content: fmt.Sprintf("run %d", i)}); v.Action != guardOK {
			t.Fatalf("run %d = %+v, want OK (different results are new information)", i, v)
		}
	}
}

func TestLoopGuardDetectsAlternatingCycle(t *testing.T) {
	g := newLoopGuard()
	read := guardCall("read_file", `{"path":"a.go","start_line":1,"end_line":5}`)
	patch := guardCall("patch_file", `{"path":"a.go","search":"x","replace":"y"}`)
	readRes := tools.Result{Content: "package a"}
	patchRes := tools.Result{Content: "patch_file: search text was not found in a.go.", IsError: true}

	var verdict guardVerdict
	for i := 0; i < loopAbortRepeats && verdict.Action != guardAbort; i++ {
		if v := g.observe(read, readRes); v.Action == guardAbort {
			verdict = v
			break
		}
		if v := g.observe(patch, patchRes); v.Action == guardAbort {
			verdict = v
			break
		}
	}
	if verdict.Action != guardAbort || verdict.Reason != "loop_detected" {
		t.Fatalf("alternating cycle = %+v, want abort loop_detected", verdict)
	}
}

func TestLoopGuardErrorStreak(t *testing.T) {
	g := newLoopGuard()
	var verdict guardVerdict
	for i := 0; i < errAbortStreak; i++ {
		call := guardCall("patch_file", fmt.Sprintf(`{"path":"a.go","search":"missing-%d","replace":"x"}`, i))
		verdict = g.observe(call, tools.Result{Content: "not found", IsError: true})
		if i == errWarnStreak-1 && verdict.Action != guardNudge {
			t.Fatalf("verdict at %d errors = %v, want nudge", i+1, verdict.Action)
		}
	}
	if verdict.Action != guardAbort || verdict.Reason != "tool_error_limit" {
		t.Fatalf("verdict at %d errors = %+v, want abort tool_error_limit", errAbortStreak, verdict)
	}
}

func TestLoopGuardErrorsResetOnSuccess(t *testing.T) {
	g := newLoopGuard()
	for i := 0; i < errAbortStreak-1; i++ {
		g.observe(guardCall("patch_file", fmt.Sprintf(`{"path":"a.go","search":"bad-%d"}`, i)), tools.Result{Content: "not found", IsError: true})
	}
	if v := g.observe(guardCall("read_file", `{"path":"a.go","start_line":1,"end_line":3}`), tools.Result{Content: "package a"}); v.Action != guardOK {
		t.Fatalf("successful call = %v, want OK", v.Action)
	}
	for i := 0; i < errAbortStreak-1; i++ {
		v := g.observe(guardCall("patch_file", fmt.Sprintf(`{"path":"a.go","search":"bad-again-%d"}`, i)), tools.Result{Content: "not found", IsError: true})
		if v.Action == guardAbort {
			t.Fatalf("error streak was not reset by the successful call")
		}
	}
}
