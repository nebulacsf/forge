package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"forge/internal/config"
	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
)

const (
	compactMinMessages      = 4
	compactPerMessageChars  = 6000
	compactToolResultChars  = 400
	compactTranscriptBudget = 120_000
	compactTimeout          = 2 * time.Minute
	compactSummaryPrefix    = "Conversation summary (context compacted):\n\n"
)

// handleCompactSession summarizes the transcript into one message and replaces
// the older history so the next turn starts with a smaller context.
func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.runs.liveRun(id) != nil {
		apiError(w, http.StatusConflict,
			"this chat has a running agent; stop it or wait for it to finish before compacting")
		return
	}
	session, err := s.store.GetSession(id)
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
	cfg := s.cfg.Get()
	if strings.TrimSpace(cfg.Providers.OpenRouter.APIKey) == "" {
		apiError(w, http.StatusBadRequest, "OpenRouter API key is not configured. Open Settings and add your key.")
		return
	}
	messages, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(messages) < compactMinMessages {
		apiError(w, http.StatusBadRequest, "There is not enough conversation to compact yet.")
		return
	}

	summary, usage, err := s.summarizeConversation(r.Context(), cfg, session, project, messages)
	if err != nil {
		logx.Warn("compact failed", "session", id, "model", session.Model, "error", err)
		apiError(w, http.StatusBadGateway, "Could not compact the conversation: "+err.Error())
		return
	}

	keep := compactKeep(messages)
	text := compactSummaryPrefix + summary
	if err := s.store.ReplaceMessages(id, text, keep); err != nil {
		logx.Error("compact persist failed", "session", id, "error", err)
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.Cost > 0 {
		if err := s.store.AddUsage(id, usage.PromptTokens, usage.CompletionTokens,
			usage.PromptTokensDetails.CachedTokens, usage.Cost); err != nil {
			logx.Error("compact usage failed", "session", id, "error", err)
		}
	}

	fresh, err := s.store.GetSession(id)
	if err != nil {
		fresh = session
	}
	updated, err := s.store.ListMessages(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Info("conversation compacted", "session", id, "model", session.Model,
		"removed", len(messages)-len(keep), "kept", len(keep),
		"summary_chars", len(summary), "prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens, "cost", usage.Cost)

	writeJSON(w, http.StatusOK, map[string]any{
		"session":  fresh,
		"messages": messageDTOs(updated),
		"summary":  text,
		"removed":  len(messages) - len(keep),
	})
}

func (s *Server) summarizeConversation(ctx context.Context, cfg config.Config, session store.Session, project store.Project, messages []store.Message) (string, llm.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()

	transcript, omitted := compactTranscript(messages)
	if strings.TrimSpace(transcript) == "" {
		return "", llm.Usage{}, fmt.Errorf("the conversation has no text to summarize")
	}
	user := fmt.Sprintf("Project: %s (%s)\nMode: %s\n\nConversation transcript:\n%s\n", project.Name, project.Path, session.Mode, transcript)
	if omitted > 0 {
		user += fmt.Sprintf("\n[%d older messages were omitted from this transcript]\n", omitted)
	}
	user += "\nWrite the summary now."

	client := llm.New(cfg.Providers.OpenRouter.APIKey, cfg.Providers.OpenRouter.BaseURL)
	result, err := client.StreamChat(ctx, llm.ChatRequest{
		Model: session.Model,
		Messages: []llm.Message{
			{Role: "system", Content: compactSystemPrompt},
			{Role: "user", Content: user},
		},
	}, nil)
	if err != nil {
		return "", llm.Usage{}, err
	}
	if result == nil {
		return "", llm.Usage{}, fmt.Errorf("the provider returned an empty response")
	}
	summary := strings.TrimSpace(result.Content)
	if summary == "" {
		summary = strings.TrimSpace(result.Reasoning)
	}
	if summary == "" {
		return "", result.Usage, fmt.Errorf("the provider returned an empty summary")
	}
	return summary, result.Usage, nil
}

const compactSystemPrompt = `You are Forge, compacting the history of a coding session so the agent can continue with a smaller context.
Preserve, in this order:
1. The user's goals and explicit constraints.
2. Decisions made and the current state of the work.
3. Files read, created, or modified, and why.
4. What tool calls found (build/test results, errors, important code facts).
5. Pending work and the recommended next step.
Do not invent facts. Omit small talk and redundant tool output. Aim for under 600 words and return only the summary.`

// compactTranscript renders messages newest-first until the budget is reached,
// then restores chronological order. Returns the transcript and how many of the
// oldest messages were omitted.
func compactTranscript(messages []store.Message) (string, int) {
	parts := make([]string, 0, len(messages))
	used := 0
	omitted := 0
	for i := len(messages) - 1; i >= 0; i-- {
		text := compactMessageText(messages[i])
		if text == "" {
			continue
		}
		if used+len(text) > compactTranscriptBudget {
			omitted = i + 1
			break
		}
		parts = append(parts, text)
		used += len(text)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n\n"), omitted
}

func compactMessageText(m store.Message) string {
	switch m.Role {
	case "user":
		// Attachments can be tens of thousands of chars; the summary only needs
		// the intent plus the list of pinned paths.
		text := stripAttachmentBlocks(m.Content)
		if text == "" {
			text = attachmentPathList(m.Content)
		}
		if text == "" {
			return ""
		}
		return "[user]\n" + clipText(text, compactPerMessageChars)
	case "assistant":
		var b strings.Builder
		if text := strings.TrimSpace(m.Content); text != "" {
			b.WriteString("[assistant]\n")
			b.WriteString(clipText(text, compactPerMessageChars))
		}
		var calls []llm.ToolCall
		if strings.TrimSpace(m.ToolCalls) != "" {
			_ = json.Unmarshal([]byte(m.ToolCalls), &calls)
		}
		if len(calls) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[tools] ")
			for i, call := range calls {
				if i > 0 {
					b.WriteString("; ")
				}
				b.WriteString(call.Function.Name)
				if result := strings.TrimSpace(call.Result); result != "" {
					b.WriteString(" -> ")
					b.WriteString(clipText(collapseSpace(result), compactToolResultChars))
				}
			}
		}
		return strings.TrimSpace(b.String())
	default:
		return ""
	}
}

// compactKeep retains the last normal user/assistant exchange so the next turn
// still has immediate context.
func compactKeep(messages []store.Message) []store.Message {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role == "user" && strings.TrimSpace(last.ToolCalls) == "" && strings.TrimSpace(last.Content) != "" {
		return []store.Message{{Role: "user", Content: last.Content}}
	}
	if len(messages) < 2 {
		return nil
	}
	prev := messages[len(messages)-2]
	if prev.Role == "user" && last.Role == "assistant" &&
		strings.TrimSpace(prev.ToolCalls) == "" && strings.TrimSpace(last.ToolCalls) == "" &&
		strings.TrimSpace(prev.Content) != "" && strings.TrimSpace(last.Content) != "" {
		return []store.Message{
			{Role: "user", Content: prev.Content},
			{Role: "assistant", Content: last.Content},
		}
	}
	return nil
}

func clipText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return text
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return string(r[:limit]) + "…"
}

func collapseSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
