package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"forge/internal/logx"
)

// callSeq correlates the request, stream chunks and assembled response of one
// provider call when several run concurrently (chat vs /compact).
var callSeq atomic.Uint64

// nextCallID returns a short, unique per-process call identifier.
func nextCallID() string {
	return fmt.Sprintf("call-%d", callSeq.Add(1))
}

// traceRequest dumps the exact JSON body sent to the provider: model, every
// message (system prompt, full history, tool results), the tool schemas and the
// tool_choice. The API key lives in the Authorization header and is never part
// of the body.
func traceRequest(callID, url string, body []byte) {
	if !logx.LLMEnabled() {
		return
	}
	logx.LLMTrace("llm request", "call", callID, "url", url, "bytes", len(body))
	logx.LLMDump("llm request "+callID, prettyJSON(body))
}

// traceResponse records the response envelope. Headers are limited to the
// useful ones; Authorization is upstream and is never logged.
func traceResponse(callID string, status int, header http.Header) {
	if !logx.LLMEnabled() {
		return
	}
	logx.LLMTrace("llm response headers", "call", callID, "status", status,
		"content_type", header.Get("Content-Type"),
		"request_id", header.Get("X-Request-Id"))
}

// traceErrorBody dumps a non-2xx provider body before it is truncated for the
// user-facing error message.
func traceErrorBody(callID string, status int, body []byte) {
	if !logx.LLMEnabled() {
		return
	}
	logx.LLMTrace("llm error response", "call", callID, "status", status, "bytes", len(body))
	logx.LLMDump(fmt.Sprintf("llm error body %s (status %d)", callID, status), prettyJSON(body))
}

// traceChunk records one raw SSE payload line, before decoding, so malformed or
// provider-specific chunks are visible too.
func traceChunk(callID, data string) {
	if !logx.LLMEnabled() {
		return
	}
	logx.LLMTrace("llm stream chunk", "call", callID, "data", data)
}

// traceResult dumps the assembled response: reasoning, content, tool calls with
// their arguments and token usage.
func traceResult(callID string, res *StreamResult) {
	if !logx.LLMEnabled() || res == nil {
		return
	}
	payload := map[string]any{
		"id":         res.ID,
		"model":      res.Model,
		"reasoning":  res.Reasoning,
		"content":    res.Content,
		"tool_calls": res.ToolCalls,
		"usage":      res.Usage,
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		logx.LLMTrace("llm response encode failed", "call", callID, "error", err)
		return
	}
	logx.LLMTrace("llm response", "call", callID,
		"content_chars", len(res.Content), "reasoning_chars", len(res.Reasoning),
		"tool_calls", len(res.ToolCalls))
	logx.LLMDump("llm response "+callID, string(body))
}

// prettyJSON re-indents a JSON document for readability, falling back to the
// raw text when the payload is not valid JSON.
func prettyJSON(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "(empty)"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, trimmed, "", "  "); err != nil {
		return string(trimmed)
	}
	return out.String()
}
