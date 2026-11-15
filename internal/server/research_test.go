package server

import (
	"strings"
	"testing"

	"forge/internal/llm"
	"forge/internal/tools"
)

func ledgerCall(name, args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.ToolFunction{Name: name, Arguments: args}}
}

func TestResearchLedgerMergesReadsAndRenders(t *testing.T) {
	ledger := newResearchLedger()
	ledger.observe(ledgerCall("read_file", `{"path":"./file.go","start_line":1,"end_line":20}`), tools.Result{})
	ledger.observe(ledgerCall("read_file", `{"path":"file.go","start_line":15,"end_line":30}`), tools.Result{})
	ledger.observe(ledgerCall("grep_search", `{"pattern":"func Save","path":"internal"}`), tools.Result{})
	ledger.rounds = 2
	ledger.calls = 3

	out := ledger.render(8, 24)
	for _, want := range []string{
		"Research budget: 2/8 tool rounds, 3/24 tool calls used",
		"read file.go L1-30",
		"grep /func Save/ in internal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger missing %q:\n%s", want, out)
		}
	}
}

func TestResearchLedgerSkipsFailedCalls(t *testing.T) {
	ledger := newResearchLedger()
	ledger.observe(ledgerCall("read_file", `{"path":"file.go","start_line":1,"end_line":20}`),
		tools.Result{IsError: true})
	out := ledger.render(8, 24)
	if strings.Contains(out, "read file.go") {
		t.Errorf("failed read was recorded:\n%s", out)
	}
}
