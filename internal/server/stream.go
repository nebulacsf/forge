package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
	"forge/internal/tools"
)

const maxToolRounds = 24

type chatEvent struct {
	Type    string `json:"type"`
	Round   int    `json:"round,omitempty"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`

	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Status     string `json:"status,omitempty"`

	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	CachedTokens     int64   `json:"cached_tokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoning_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	ContextTokens    int64   `json:"context_tokens,omitempty"`
	SessionTokensIn  int64   `json:"session_tokens_in,omitempty"`
	SessionTokensOut int64   `json:"session_tokens_out,omitempty"`
	SessionCostUSD   float64 `json:"session_cost_usd,omitempty"`
	ProjectCostUSD   float64 `json:"project_cost_usd,omitempty"`

	Checklist     *[]store.ChecklistItem `json:"checklist,omitempty"`
	Files         []tools.FileAction     `json:"files,omitempty"`
	Session       *store.Session         `json:"session,omitempty"`
	ReasoningOnly bool                   `json:"reasoning_only,omitempty"`
}

type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s *sseWriter) send(ev chatEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) fail(msg string) {
	_ = s.send(chatEvent{Type: "error", Message: msg})
}

// handleChatStream runs a complete agent turn: it streams the model response,
// executes tool calls, appends history, and emits typed SSE events.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID int64  `json:"session_id"`
		Content   string `json:"content"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SessionID <= 0 {
		apiError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" {
		apiError(w, http.StatusBadRequest, "message cannot be empty")
		return
	}
	if utf8.RuneCountInString(body.Content) > 200_000 {
		apiError(w, http.StatusBadRequest, "message is too large")
		return
	}

	session, err := s.store.GetSession(body.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	project, err := s.store.GetProject(session.ProjectID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if info, statErr := statDir(project.Path); statErr != nil || !info {
		apiError(w, http.StatusBadRequest, "project directory no longer exists: "+project.Path)
		return
	}

	cfg := s.cfg.Get()
	if strings.TrimSpace(cfg.Providers.OpenRouter.APIKey) == "" {
		apiError(w, http.StatusBadRequest, "OpenRouter API key is not configured. Open Settings and add your key.")
		return
	}
	if strings.TrimSpace(session.Model) == "" {
		session.Model = cfg.Defaults.Model
		if updated, err := s.store.UpdateSession(session.ID, nil, &session.Model, nil, nil); err == nil {
			session = updated
		}
	}
	if session.Mode != "BUILD" {
		session.Mode = "PLAN"
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	stream := &sseWriter{w: w, rc: rc}
	ctx := r.Context()
	turnStart := time.Now()
	logx.Info("turn start", "session", session.ID, "project", project.ID, "model", session.Model, "mode", session.Mode)

	// Persist the user message first.
	if _, err := s.store.AddMessage(session.ID, "user", "", body.Content, ""); err != nil {
		stream.fail("Could not save message: " + err.Error())
		return
	}
	_ = s.store.TouchProject(project.ID)

	if session.Title == "" || session.Title == "New Chat" {
		title := deriveTitle(body.Content)
		if updated, err := s.store.UpdateSession(session.ID, &title, nil, nil, nil); err == nil {
			session = updated
			stream.send(chatEvent{Type: "session", Session: &session})
		}
	}

	mgr, err := tools.NewManager(project.Path, cfg.Defaults.MaxFileReadChars,
		time.Duration(cfg.Defaults.BashTimeoutSec)*time.Second, s.store, session.ID)
	if err != nil {
		stream.fail("Project directory error: " + err.Error())
		return
	}
	mgr.Mode = session.Mode

	client := llm.New(cfg.Providers.OpenRouter.APIKey, cfg.Providers.OpenRouter.BaseURL)
	specs := tools.Specs(session.Mode)

	for round := 1; round <= maxToolRounds; round++ {
		if ctx.Err() != nil {
			s.finishCancelled(stream, session.ID, "", "")
			return
		}
		if err := stream.send(chatEvent{Type: "round_start", Round: round}); err != nil {
			s.persistPartial(session.ID, "", "")
			return
		}

		checklist, _ := s.store.ListChecklist(session.ID)
		system := buildSystemPrompt(project, session, checklist)
		history, err := s.buildHistory(session.ID, system)
		if err != nil {
			stream.fail("Could not load conversation: " + err.Error())
			return
		}
		req := llm.ChatRequest{
			Model:    session.Model,
			Messages: history,
			Tools:    specs,
		}
		logx.Debug("round request", "session", session.ID, "round", round,
			"messages", len(history), "tools", len(specs))

		result, streamErr := client.StreamChat(ctx, req, func(d llm.Delta) error {
			kind := d.Kind
			if kind != "reasoning" {
				kind = "content"
			}
			return stream.send(chatEvent{Type: kind, Round: round, Content: d.Text})
		})

		if streamErr != nil {
			if ctx.Err() != nil || errors.Is(streamErr, context.Canceled) {
				if result != nil {
					s.persistPartial(session.ID, result.Reasoning, result.Content)
				}
				logx.Info("turn cancelled", "session", session.ID, "round", round,
					"duration", time.Since(turnStart).Round(time.Millisecond).String())
				s.finishCancelled(stream, session.ID, "", "")
				return
			}
			if result != nil {
				s.persistPartial(session.ID, result.Reasoning, result.Content)
			}
			msg := streamErr.Error()
			if errors.Is(streamErr, context.DeadlineExceeded) {
				msg = "The model request timed out."
			}
			logx.Warn("provider stream failed", "session", session.ID, "round", round, "error", msg)
			stream.fail(msg)
			s.sendDone(stream, session.ID, false)
			return
		}
		if result == nil {
			logx.Warn("provider returned no result", "session", session.ID, "round", round)
			stream.fail("The provider returned an empty response.")
			s.sendDone(stream, session.ID, false)
			return
		}

		if result.Usage.PromptTokens > 0 || result.Usage.CompletionTokens > 0 || result.Usage.Cost > 0 {
			if err := s.store.AddUsage(session.ID,
				result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.Cost); err != nil {
				logx.Error("persist usage failed", "session", session.ID, "error", err)
			}
		}
		logx.Debug("round finished", "session", session.ID, "round", round,
			"content_chars", len(result.Content), "reasoning_chars", len(result.Reasoning),
			"tool_calls", len(result.ToolCalls), "prompt_tokens", result.Usage.PromptTokens,
			"completion_tokens", result.Usage.CompletionTokens, "cost", result.Usage.Cost)
		if fresh, err := s.store.GetSession(session.ID); err == nil {
			session = fresh
		}
		projectCost := project.TotalCostUSD
		if p, err := s.store.GetProject(project.ID); err == nil {
			projectCost = p.TotalCostUSD
		}
		if err := stream.send(chatEvent{
			Type:             "usage",
			Round:            round,
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
			CachedTokens:     result.Usage.PromptTokensDetails.CachedTokens,
			ReasoningTokens:  result.Usage.CompletionTokensDetails.ReasoningTokens,
			CostUSD:          result.Usage.Cost,
			ContextTokens:    result.Usage.PromptTokens + result.Usage.CompletionTokens,
			SessionTokensIn:  session.TokensIn,
			SessionTokensOut: session.TokensOut,
			SessionCostUSD:   session.CostUSD,
			ProjectCostUSD:   projectCost,
		}); err != nil {
			s.persistPartial(session.ID, result.Reasoning, result.Content)
			return
		}

		if len(result.ToolCalls) == 0 {
			hasContent := strings.TrimSpace(result.Content) != ""
			hasReasoning := strings.TrimSpace(result.Reasoning) != ""
			if !hasContent && !hasReasoning {
				logx.Warn("empty completion", "session", session.ID, "round", round, "model", session.Model)
				stream.fail("The model returned an empty response. Try another model or send the message again.")
				s.sendDone(stream, session.ID, false)
				return
			}
			if _, err := s.store.AddMessage(session.ID, "assistant", result.Reasoning, result.Content, ""); err != nil {
				logx.Error("persist assistant response failed", "session", session.ID, "error", err)
				stream.fail("Could not save response: " + err.Error())
				return
			}
			if !hasContent && hasReasoning {
				logx.Warn("model returned reasoning without a final answer",
					"session", session.ID, "round", round, "model", session.Model)
			}
			logx.Info("turn done", "session", session.ID, "rounds", round,
				"reasoning_only", !hasContent && hasReasoning,
				"duration", time.Since(turnStart).Round(time.Millisecond).String(),
				"tokens_in", session.TokensIn, "tokens_out", session.TokensOut, "cost", session.CostUSD)
			s.sendDone(stream, session.ID, !hasContent && hasReasoning)
			return
		}

		calls := result.ToolCalls
		for i := range calls {
			call := &calls[i]
			if call.ID == "" {
				call.ID = fmt.Sprintf("call_%d_%d", round, i)
			}
			if call.Type == "" {
				call.Type = "function"
			}
			if err := stream.send(chatEvent{
				Type: "tool_start", Round: round, ID: call.ID, Name: call.Function.Name,
				Arguments: call.Function.Arguments, Status: "running",
			}); err != nil {
				s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls)
				return
			}
			started := time.Now()
			toolResult := mgr.Execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
			elapsed := time.Since(started).Milliseconds()
			if toolResult.IsError {
				logx.Warn("tool failed", "session", session.ID, "round", round, "tool", call.Function.Name,
					"duration_ms", elapsed, "error", logx.Preview(toolResult.Content, 200))
			} else {
				logx.Debug("tool finished", "session", session.ID, "round", round, "tool", call.Function.Name,
					"duration_ms", elapsed, "result_chars", len(toolResult.Content))
			}
			call.Result = toolResult.Content
			call.Status = "done"
			if toolResult.IsError {
				call.Status = "error"
			}
			call.DurationMS = elapsed
			if err := stream.send(chatEvent{
				Type: "tool_end", Round: round, ID: call.ID, Name: call.Function.Name,
				Result: toolResult.Content, IsError: toolResult.IsError, DurationMS: elapsed,
				Status: call.Status, Files: toolResult.Files,
			}); err != nil {
				s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls)
				return
			}
			if toolResult.ChecklistChanged {
				if items, err := s.store.ListChecklist(session.ID); err == nil {
					stream.send(chatEvent{Type: "checklist", Checklist: &items})
				}
			}
		}

		if _, err := s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls); err != nil {
			stream.fail("Could not save tool results: " + err.Error())
			return
		}
		if ctx.Err() != nil {
			s.finishCancelled(stream, session.ID, "", "")
			return
		}
	}

	logx.Warn("tool round limit reached", "session", session.ID, "limit", maxToolRounds)
	stream.fail(fmt.Sprintf("Stopped after %d tool-call rounds. Send another message to continue.", maxToolRounds))
	s.sendDone(stream, session.ID, false)
}

func (s *Server) persistPartial(sessionID int64, reasoning, content string) {
	if strings.TrimSpace(reasoning) == "" && strings.TrimSpace(content) == "" {
		return
	}
	if _, err := s.store.AddMessage(sessionID, "assistant", reasoning, content, ""); err != nil {
		logx.Error("persist partial response failed", "session", sessionID, "error", err)
	}
}

func (s *Server) persistToolCalls(sessionID int64, reasoning, content string, calls []llm.ToolCall) (int64, error) {
	raw, err := json.Marshal(calls)
	if err != nil {
		return 0, err
	}
	msg, err := s.store.AddMessage(sessionID, "assistant", reasoning, content, string(raw))
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

func (s *Server) finishCancelled(stream *sseWriter, sessionID int64, reasoning, content string) {
	s.persistPartial(sessionID, reasoning, content)
	_ = stream.send(chatEvent{Type: "cancelled", Message: "Stopped"})
	s.sendDone(stream, sessionID, false)
}

func (s *Server) sendDone(stream *sseWriter, sessionID int64, reasoningOnly bool) {
	var ptr *store.Session
	if sess, err := s.store.GetSession(sessionID); err == nil {
		ptr = &sess
	}
	_ = stream.send(chatEvent{Type: "done", Session: ptr, ReasoningOnly: reasoningOnly})
}

func statDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// buildHistory reconstructs OpenAI-compatible messages from persisted rows.
func (s *Server) buildHistory(sessionID int64, system string) ([]llm.Message, error) {
	rows, err := s.store.ListMessages(sessionID)
	if err != nil {
		return nil, err
	}
	out := []llm.Message{{Role: "system", Content: system}}
	for _, m := range rows {
		switch m.Role {
		case "user":
			out = append(out, llm.Message{Role: "user", Content: m.Content})
		case "assistant":
			var calls []llm.ToolCall
			if strings.TrimSpace(m.ToolCalls) != "" {
				_ = json.Unmarshal([]byte(m.ToolCalls), &calls)
			}
			for i := range calls {
				if calls[i].ID == "" {
					calls[i].ID = fmt.Sprintf("call_%d_%d", m.ID, i)
				}
				if calls[i].Type == "" {
					calls[i].Type = "function"
				}
			}
			wire := make([]llm.ToolCall, 0, len(calls))
			for _, c := range calls {
				if c.Function.Name == "" {
					continue
				}
				wire = append(wire, llm.ToolCall{
					ID: c.ID, Type: c.Type,
					Function: llm.ToolFunction{Name: c.Function.Name, Arguments: c.Function.Arguments},
				})
			}
			if strings.TrimSpace(m.Content) == "" && len(wire) == 0 {
				continue
			}
			out = append(out, llm.Message{Role: "assistant", Content: m.Content, ToolCalls: wire})
			for _, c := range calls {
				if c.Function.Name == "" {
					continue
				}
				result := c.Result
				if c.Status == "error" {
					result = "ERROR: " + result
				}
				if strings.TrimSpace(result) == "" {
					result = "(no result recorded)"
				}
				out = append(out, llm.Message{Role: "tool", ToolCallID: c.ID, Name: c.Function.Name, Content: result})
			}
		}
	}
	return out, nil
}

func buildSystemPrompt(project store.Project, session store.Session, checklist []store.ChecklistItem) string {
	var b strings.Builder
	b.WriteString("You are Forge, an expert autonomous coding agent operating on a local project through a web UI.\n\n")
	fmt.Fprintf(&b, "Project: %s\nWorking directory: %s\n", project.Name, project.Path)
	fmt.Fprintf(&b, "Mode: %s\n", session.Mode)
	if session.Mode == "PLAN" {
		b.WriteString("PLAN mode is READ-ONLY. You may inspect files, search, list directories, run git diff, and update the checklist. You cannot write files or run shell commands.\n")
		b.WriteString("When you have enough information, finish with a concise numbered plan of the exact changes needed. Always put that plan in your normal response content, never only in reasoning. End the response with a line exactly like: \"Ready to implement — switch to BUILD to apply this plan.\" Never claim that files were changed or that commands were run in PLAN mode.\n")
	} else {
		b.WriteString("BUILD mode allows reading, writing, patching, and running shell commands. Make the requested changes directly, then verify them.\n")
	}
	b.WriteString(`
Operating rules:
- Stay strictly inside the project directory. Never attempt to access paths outside it.
- Read relevant files before editing. Prefer patch_file for small, surgical changes and write_file for new or fully rewritten files.
- Read both the surrounding code and existing patterns before making changes; match local style.
- Use update_checklist for multi-step tasks, and keep item statuses accurate as you work.
- Use grep_search/list_directory to explore rather than guessing file contents.
- Run focused tests or builds when appropriate. Never claim a test passed unless you ran it.
- Tool output and reasoning are rendered as collapsed accordions in the UI, so do not repeat large tool output in your final answer.
- Be concise. When finished, summarize what changed, what was verified, and any remaining risks.
`)
	if len(checklist) > 0 {
		b.WriteString("\nCurrent checklist:\n")
		for _, it := range checklist {
			mark := "[ ]"
			if it.IsDone {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "%s #%d %s\n", mark, it.ID, it.TaskText)
		}
	}
	return strings.TrimSpace(b.String())
}

func deriveTitle(content string) string {
	flat := strings.Join(strings.Fields(content), " ")
	if flat == "" {
		return "New Chat"
	}
	r := []rune(flat)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return string(r)
}
