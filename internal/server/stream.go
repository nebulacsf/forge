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

	"forge/internal/config"
	"forge/internal/llm"
	"forge/internal/logx"
	"forge/internal/store"
	"forge/internal/tools"
)

const (
	defaultSegmentRounds = 24
	defaultTurnRounds    = 96
	defaultPlanRounds    = 16
	defaultPlanToolCalls = 64
	planFinalGraceRounds = 4
	planFinalRetries     = 2
	planMinResultChars   = 8000
	continuePrompt       = "Continue from where you left off."
	continuationNote     = "You are continuing a long task from a checkpoint. Resume from the current checklist and conversation. " +
		"Do not repeat completed work or re-read files you have already read. Keep going until the task is actually finished."
)

type chatEvent struct {
	Type        string `json:"type"`
	Round       int    `json:"round,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Segment     int    `json:"segment,omitempty"`
	SegmentSize int    `json:"segment_size,omitempty"`
	NextRound   int    `json:"next_round,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Content     string `json:"content,omitempty"`
	Message     string `json:"message,omitempty"`

	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Status     string `json:"status,omitempty"`

	PromptTokens        int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens    int64   `json:"completion_tokens,omitempty"`
	TotalTokens         int64   `json:"total_tokens,omitempty"`
	CachedTokens        int64   `json:"cached_tokens,omitempty"`
	ReasoningTokens     int64   `json:"reasoning_tokens,omitempty"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
	ContextTokens       int64   `json:"context_tokens,omitempty"`
	SessionTokensIn     int64   `json:"session_tokens_in,omitempty"`
	SessionTokensOut    int64   `json:"session_tokens_out,omitempty"`
	SessionCachedTokens int64   `json:"session_cached_tokens,omitempty"`
	SessionCostUSD      float64 `json:"session_cost_usd,omitempty"`
	ProjectCostUSD      float64 `json:"project_cost_usd,omitempty"`
	// History pruning snapshot for the request that produced this usage
	// event: total tool rounds in the session history, how many were stubbed,
	// and how many characters the stubs/clipping saved.
	HistoryRounds int `json:"history_rounds,omitempty"`
	PrunedRounds  int `json:"pruned_rounds,omitempty"`
	PrunedChars   int `json:"pruned_chars,omitempty"`

	Checklist     *[]store.ChecklistItem `json:"checklist,omitempty"`
	QuestionBatch *store.QuestionBatch   `json:"question_batch,omitempty"`
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
		SessionID   int64    `json:"session_id"`
		Content     string   `json:"content"`
		Continue    bool     `json:"continue"`
		Attachments []string `json:"attachments"`
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
	if body.Continue && body.Content == "" {
		body.Content = continuePrompt
	}
	if body.Content == "" && len(body.Attachments) == 0 {
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

	// Files pinned with the composer's "@" picker are inlined into the persisted
	// user message so the model sees them and later turns keep the context.
	userContent := body.Content
	if block := buildAttachmentBlock(project.Path, body.Attachments); block != "" {
		if userContent == "" {
			userContent = "Attached context."
		}
		userContent += "\n\n" + block
	}

	// Persist the user message before the run starts so the transcript stays
	// consistent even if the browser disappears right after sending.
	if _, err := s.store.AddMessage(session.ID, "user", "", userContent, ""); err != nil {
		apiError(w, http.StatusInternalServerError, "Could not save message: "+err.Error())
		return
	}
	_ = s.store.TouchProject(project.ID)

	var announce *store.Session
	if session.Title == "" || session.Title == "New Chat" {
		titleSource := body.Content
		if strings.TrimSpace(titleSource) == "" && len(body.Attachments) > 0 {
			titleSource = "Attach " + strings.Join(body.Attachments, ", ")
		}
		title := deriveTitle(titleSource)
		if updated, err := s.store.UpdateSession(session.ID, &title, nil, nil, nil); err == nil {
			session = updated
			announce = &session
		}
	}

	// Admission: at most one BUILD agent per project, any number of PLAN agents.
	rn, err := s.runs.start(session, project, s.maxConcurrentRuns())
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	go s.runTurn(rn, session, project, announce, cfg)
	s.tailRun(w, r, rn, 0)
}

// runTurn is the agent loop. It runs on the run's own context, so the browser
// can go away without stopping the turn, and it publishes every event into the
// run log instead of writing to the HTTP socket.
func (s *Server) runTurn(rn *run, session store.Session, project store.Project, announce *store.Session, cfg config.Config) {
	stream := &runPublisher{rn: rn}
	ctx := rn.ctx
	turnStart := time.Now()
	defer rn.finish("turn_end")
	logx.Info("turn start", "session", session.ID, "project", project.ID, "model", session.Model, "mode", session.Mode)
	if announce != nil {
		stream.send(chatEvent{Type: "session", Session: announce})
	}

	// A turn may span several rounds and end from many branches. Record the
	// wall-clock duration once, before the final session snapshot is sent, so
	// the UI can show both the per-response timer and the session total.
	var lastAssistantID int64
	timingSaved := false
	saveTurnTiming := func() {
		if timingSaved {
			return
		}
		timingSaved = true
		elapsed := time.Since(turnStart).Milliseconds()
		if lastAssistantID > 0 {
			if err := s.store.SetMessageDuration(lastAssistantID, elapsed); err != nil {
				logx.Error("persist message duration failed", "session", session.ID, "error", err)
			}
		}
		if err := s.store.AddSessionDuration(session.ID, elapsed); err != nil {
			logx.Error("persist session duration failed", "session", session.ID, "error", err)
		}
	}
	finish := func(reasoningOnly bool) {
		saveTurnTiming()
		s.sendDone(stream, session.ID, reasoningOnly)
	}
	cancelTurn := func(reasoning, content string) {
		if id := s.persistPartial(session.ID, reasoning, content); id > 0 {
			lastAssistantID = id
		}
		_ = stream.send(chatEvent{Type: "cancelled", Message: "Stopped"})
		finish(false)
	}

	mgr, err := tools.NewManager(project.Path, cfg.Defaults.MaxFileReadChars,
		time.Duration(cfg.Defaults.BashTimeoutSec)*time.Second, s.store, session.ID)
	if err != nil {
		stream.fail("Project directory error: " + err.Error())
		return
	}
	mgr.Mode = session.Mode
	mgr.ProjectID = project.ID

	// Build the local repository outline and symbol index once per turn.
	// Subsequent rounds re-render the outline from SQLite so writes made by
	// tools are visible immediately.
	outline, outlineErr := tools.EnsureProjectIndex(ctx, s.store, project.ID, project.Path)
	if outlineErr != nil {
		logx.Warn("repo outline unavailable", "session", session.ID, "project", project.ID, "error", outlineErr)
	} else {
		logx.Debug("repo outline ready", "session", session.ID, "project", project.ID,
			"chars", utf8.RuneCountInString(outline))
	}

	client := llm.New(cfg.Providers.OpenRouter.APIKey, cfg.Providers.OpenRouter.BaseURL)
	specs := tools.Specs(session.Mode)

	segmentSize := cfg.Defaults.MaxToolRounds
	if segmentSize <= 0 {
		segmentSize = defaultSegmentRounds
	}
	turnLimit := cfg.Defaults.MaxTurnRounds
	if turnLimit <= 0 {
		turnLimit = defaultTurnRounds
	}
	if turnLimit < segmentSize {
		turnLimit = segmentSize
	}
	planRounds := cfg.Defaults.PlanRounds
	if planRounds <= 0 {
		planRounds = defaultPlanRounds
	}
	planToolCalls := cfg.Defaults.PlanToolCalls
	if planToolCalls <= 0 {
		planToolCalls = defaultPlanToolCalls
	}
	autoContinue := cfg.Defaults.AutoContinue == nil || *cfg.Defaults.AutoContinue
	turnCostLimit := cfg.Defaults.MaxTurnCostUSD

	// PLAN is a single bounded research turn. It never auto-continues: when the
	// research budget is spent, tools are disabled and the model must answer.
	// The extra grace rounds cover the forced final answer and its retries.
	if session.Mode == "PLAN" {
		if planRounds < 1 {
			planRounds = 1
		}
		turnLimit = planRounds + planFinalGraceRounds
		segmentSize = turnLimit
		autoContinue = false
	}

	totalRounds := 0
	segment := 1
	turnCost := 0.0
	toolRounds := 0
	toolCalls := 0
	finalizing := false
	finalAttempts := 0
	finalReason := ""
	var continueNote, guardNote string
	ledger := newResearchLedger()
	guard := newLoopGuard()

	for {
		var abortReason, abortMessage string
		for segRound := 1; segRound <= segmentSize && totalRounds < turnLimit; segRound++ {
			totalRounds++
			if ctx.Err() != nil {
				cancelTurn("", "")
				return
			}
			if err := stream.send(chatEvent{
				Type: "round_start", Round: totalRounds, Limit: turnLimit,
				Segment: segment, SegmentSize: segmentSize,
			}); err != nil {
				saveTurnTiming()
				return
			}

			if totalRounds > 1 {
				if fresh, err := tools.CurrentRepoOutline(s.store, project.Path); err == nil {
					outline = fresh
				}
			}

			// The PLAN research budget is a hard stop. Once it is spent the next
			// round is the final answer: tools stay in the request for prompt
			// cache stability, but tool_choice=none and the server ignores any
			// tool call the provider still emits.
			if session.Mode == "PLAN" && !finalizing &&
				(toolRounds >= planRounds || toolCalls >= planToolCalls) {
				finalizing = true
				finalAttempts = 0
				finalReason = "research budget exhausted"
				logx.Info("plan research budget reached", "session", session.ID,
					"round", totalRounds, "tool_rounds", toolRounds, "tool_calls", toolCalls,
					"plan_rounds", planRounds, "plan_tool_calls", planToolCalls)
			}
			if finalizing {
				finalAttempts++
			}

			checklist, _ := s.store.ListChecklist(session.ID)
			system := buildSystemPrompt(project, session, segmentSize, turnLimit, planRounds, planToolCalls)
			rows, err := s.store.ListMessages(session.ID)
			if err != nil {
				stream.fail("Could not load conversation: " + err.Error())
				saveTurnTiming()
				return
			}
			keepRecent := cfg.Defaults.HistoryKeepRecent
			// In PLAN, the current turn is the research budget. Keep every tool
			// round of this turn verbatim so pruning cannot erase the findings
			// that the model needs for the final plan. Older turns are pruned
			// normally; the budget guarantees the current turn stays small.
			planFloor := 0
			if session.Mode == "PLAN" {
				planFloor = planRounds + 2
				if planFloor > keepRecent {
					keepRecent = planFloor
				}
			}
			maxResult := effectiveHistoryResultLimit(session.Mode, cfg.Defaults.HistoryMaxResult)
			// Volatile state and steering notes are ephemeral: they live at the
			// very end of the request so the stable prefix stays cacheable and
			// the persisted conversation is never polluted. They are built before
			// the history so the token budget can reserve room for them.
			var tail []string
			if continueNote != "" {
				tail = append(tail, continueNote)
				continueNote = ""
			}
			if state := buildStateMessage(checklist, outline); state != "" {
				tail = append(tail, state)
			}
			if session.Mode == "PLAN" {
				ledger.rounds = toolRounds
				ledger.calls = toolCalls
				if research := ledger.render(planRounds, planToolCalls); research != "" {
					tail = append(tail, research)
				}
			}
			if guardNote != "" {
				tail = append(tail, guardNote)
				guardNote = ""
			}
			noTools := finalizing && finalAttempts > planFinalRetries
			if finalizing {
				reason := finalReason
				if reason == "" {
					reason = "research budget exhausted"
				}
				tail = append(tail, buildFinalPlanNote(reason, planRounds, planToolCalls,
					toolRounds, toolCalls, finalAttempts, noTools))
			}
			tailText := strings.Join(tail, "\n\n")
			// Budget-driven pruning: min(context length * percent, max tokens).
			// A zero budget means the user disabled it, so the legacy round-based
			// knobs decide the window instead. ReservedChars covers everything the
			// budget must also pay for: tool definitions and the ephemeral tail.
			tokenBudget := cfg.Defaults.PruneTokenBudget(llm.BuiltinContextLength(session.Model))
			reservedChars := toolSpecsChars(specs) + utf8.RuneCountInString(tailText)
			history, hstats := buildHistory(rows, system, historyOptions{
				KeepRecent:     keepRecent,
				MaxResultChars: maxResult,
				MaxArgChars:    cfg.Defaults.HistoryMaxArg,
				ChunkRounds:    cfg.Defaults.HistoryPruneChunk,
				TokenBudget:    tokenBudget,
				ReservedChars:  reservedChars,
				MinRecent:      planFloor,
			})
			logx.Debug("history built", "session", session.ID, "round", totalRounds,
				"messages", hstats.Messages, "chars", hstats.Chars,
				"est_tokens", hstats.Chars/4, "pruned_rounds", hstats.PrunedRounds,
				"pruned_chars", hstats.PrunedChars, "keep_recent", keepRecent,
				"max_result_chars", maxResult, "token_budget", tokenBudget,
				"reserved_chars", reservedChars)
			if tailText != "" {
				history = append(history, llm.Message{Role: "user", Content: tailText})
			}
			req := llm.ChatRequest{
				Model:    session.Model,
				Messages: history,
				Tools:    specs,
			}
			if finalizing {
				if noTools {
					// Last resort: remove the tool definitions entirely so a
					// provider that ignores tool_choice cannot keep calling tools.
					req.Tools = nil
					req.ToolChoice = ""
				} else {
					req.ToolChoice = "none"
				}
			}
			logx.Debug("round request", "session", session.ID, "round", totalRounds,
				"segment", segment, "messages", len(history), "tools", len(req.Tools),
				"finalizing", finalizing, "tool_choice", req.ToolChoice, "no_tools", noTools)

			result, streamErr := client.StreamChat(ctx, req, func(d llm.Delta) error {
				kind := d.Kind
				if kind != "reasoning" {
					kind = "content"
				}
				return stream.send(chatEvent{Type: kind, Round: totalRounds, Content: d.Text})
			})

			if streamErr != nil {
				if ctx.Err() != nil || errors.Is(streamErr, context.Canceled) {
					logx.Info("turn cancelled", "session", session.ID, "round", totalRounds,
						"duration", time.Since(turnStart).Round(time.Millisecond).String())
					if result != nil {
						cancelTurn(result.Reasoning, result.Content)
					} else {
						cancelTurn("", "")
					}
					return
				}
				if result != nil {
					if id := s.persistPartial(session.ID, result.Reasoning, result.Content); id > 0 {
						lastAssistantID = id
					}
				}
				msg := streamErr.Error()
				if errors.Is(streamErr, context.DeadlineExceeded) {
					msg = "The model request timed out."
				}
				if looksLikeContextOverflow(msg) {
					msg += " The conversation is too large for this model; run /compact to summarize it and continue."
				}
				logx.Warn("provider stream failed", "session", session.ID, "round", totalRounds, "error", msg)
				stream.fail(msg)
				finish(false)
				return
			}
			if result == nil {
				logx.Warn("provider returned no result", "session", session.ID, "round", totalRounds)
				stream.fail("The provider returned an empty response.")
				finish(false)
				return
			}

			if result.Usage.PromptTokens > 0 || result.Usage.CompletionTokens > 0 || result.Usage.Cost > 0 {
				if err := s.store.AddUsage(session.ID,
					result.Usage.PromptTokens, result.Usage.CompletionTokens,
					result.Usage.PromptTokensDetails.CachedTokens, result.Usage.Cost); err != nil {
					logx.Error("persist usage failed", "session", session.ID, "error", err)
				}
			}
			turnCost += result.Usage.Cost
			cacheRatio := 0.0
			if result.Usage.PromptTokens > 0 {
				cacheRatio = float64(result.Usage.PromptTokensDetails.CachedTokens) / float64(result.Usage.PromptTokens)
			}
			logx.Debug("round finished", "session", session.ID, "round", totalRounds,
				"content_chars", len(result.Content), "reasoning_chars", len(result.Reasoning),
				"tool_calls", len(result.ToolCalls), "prompt_tokens", result.Usage.PromptTokens,
				"cached_tokens", result.Usage.PromptTokensDetails.CachedTokens,
				"cache_ratio", fmt.Sprintf("%.2f", cacheRatio),
				"completion_tokens", result.Usage.CompletionTokens, "cost", result.Usage.Cost)
			if fresh, err := s.store.GetSession(session.ID); err == nil {
				session = fresh
			}
			projectCost := project.TotalCostUSD
			if p, err := s.store.GetProject(project.ID); err == nil {
				projectCost = p.TotalCostUSD
			}
			if err := stream.send(chatEvent{
				Type:                "usage",
				Round:               totalRounds,
				PromptTokens:        result.Usage.PromptTokens,
				CompletionTokens:    result.Usage.CompletionTokens,
				TotalTokens:         result.Usage.TotalTokens,
				CachedTokens:        result.Usage.PromptTokensDetails.CachedTokens,
				ReasoningTokens:     result.Usage.CompletionTokensDetails.ReasoningTokens,
				CostUSD:             result.Usage.Cost,
				ContextTokens:       result.Usage.PromptTokens + result.Usage.CompletionTokens,
				SessionTokensIn:     session.TokensIn,
				SessionTokensOut:    session.TokensOut,
				SessionCachedTokens: session.CachedTokens,
				SessionCostUSD:      session.CostUSD,
				ProjectCostUSD:      projectCost,
				HistoryRounds:       hstats.Rounds,
				PrunedRounds:        hstats.PrunedRounds,
				PrunedChars:         hstats.PrunedChars,
			}); err != nil {
				if id := s.persistPartial(session.ID, result.Reasoning, result.Content); id > 0 {
					lastAssistantID = id
				}
				saveTurnTiming()
				return
			}

			if finalizing {
				hasReasoning := strings.TrimSpace(result.Reasoning) != ""
				if len(result.ToolCalls) > 0 {
					// tool_choice=none should prevent this, but a provider may still
					// emit calls. They are deliberately ignored: executing one would
					// restart the loop the budget is meant to stop.
					logx.Warn("plan finalization ignored tool calls", "session", session.ID,
						"round", totalRounds, "calls", len(result.ToolCalls))
				}
				if !usablePlanContent(result.Content) {
					// Do not persist the bad content: some providers return a
					// <tool_call> block as text when tools are disabled. Persisting
					// it would show a fake tool call as the plan and would also feed
					// it back to the model on the retry.
					logx.Warn("plan finalization returned unusable content",
						"session", session.ID, "round", totalRounds, "attempt", finalAttempts,
						"content_chars", len(result.Content),
						"preview", logx.Preview(result.Content, 200))
					if id := s.persistPartial(session.ID, result.Reasoning, ""); id > 0 {
						lastAssistantID = id
					}
					if finalAttempts <= planFinalRetries {
						logx.Warn("plan finalization retrying with a stricter instruction",
							"session", session.ID, "round", totalRounds, "attempt", finalAttempts)
						continue
					}
					if hasReasoning {
						logx.Warn("model returned reasoning without a final plan",
							"session", session.ID, "round", totalRounds, "model", session.Model)
						finish(true)
						return
					}
					stream.fail("The model did not return a usable plan after the PLAN research budget.")
					finish(false)
					return
				}
				msg, err := s.store.AddMessage(session.ID, "assistant", result.Reasoning, result.Content, "")
				if err != nil {
					logx.Error("persist plan failed", "session", session.ID, "error", err)
					stream.fail("Could not save the plan: " + err.Error())
					saveTurnTiming()
					return
				}
				lastAssistantID = msg.ID
				logx.Info("plan finalized", "session", session.ID, "round", totalRounds,
					"reason", finalReason, "tool_rounds", toolRounds, "tool_calls", toolCalls,
					"duration", time.Since(turnStart).Round(time.Millisecond).String())
				finish(false)
				return
			}

			if len(result.ToolCalls) == 0 {
				if session.Mode == "PLAN" && looksLikePseudoToolCall(result.Content) {
					// The model wrote a tool call into the response text instead
					// of using the native interface. Treat it as malformed, not as
					// a final answer, and give it one corrective round.
					logx.Warn("model wrote a tool call as text", "session", session.ID,
						"round", totalRounds, "preview", logx.Preview(result.Content, 200))
					if id := s.persistPartial(session.ID, result.Reasoning, ""); id > 0 {
						lastAssistantID = id
					}
					// A malformed tool call is still an attempt to use the research
					// budget. Count it so a model that keeps emitting text tool calls
					// reaches the forced finalization instead of consuming grace rounds.
					toolRounds++
					toolCalls++
					if totalRounds < turnLimit {
						guardNote = "Invalid response: tool calls must use the native tool interface. " +
							"Do not write <tool_call>, <function=...>, XML, or JSON tool calls in the response text. " +
							"Use the native tools, or write the final plan if the budget is exhausted."
						continue
					}
					stream.fail("The model wrote a tool call as text instead of using the tool interface.")
					finish(false)
					return
				}
				hasContent := strings.TrimSpace(result.Content) != ""
				hasReasoning := strings.TrimSpace(result.Reasoning) != ""
				if !hasContent && !hasReasoning {
					logx.Warn("empty completion", "session", session.ID, "round", totalRounds, "model", session.Model)
					stream.fail("The model returned an empty response. Try another model or send the message again.")
					finish(false)
					return
				}
				msg, err := s.store.AddMessage(session.ID, "assistant", result.Reasoning, result.Content, "")
				if err != nil {
					logx.Error("persist assistant response failed", "session", session.ID, "error", err)
					stream.fail("Could not save response: " + err.Error())
					saveTurnTiming()
					return
				}
				lastAssistantID = msg.ID
				if !hasContent && hasReasoning {
					logx.Warn("model returned reasoning without a final answer",
						"session", session.ID, "round", totalRounds, "model", session.Model)
				}
				logx.Info("turn done", "session", session.ID, "rounds", totalRounds, "segments", segment,
					"reasoning_only", !hasContent && hasReasoning,
					"duration", time.Since(turnStart).Round(time.Millisecond).String(),
					"tokens_in", session.TokensIn, "tokens_out", session.TokensOut, "cost", session.CostUSD)
				finish(!hasContent && hasReasoning)
				return
			}

			calls := result.ToolCalls
			var askedBatches []*store.QuestionBatch
			for i := range calls {
				call := &calls[i]
				if call.ID == "" {
					call.ID = fmt.Sprintf("call_%d_%d", totalRounds, i)
				}
				if call.Type == "" {
					call.Type = "function"
				}
				toolCalls++
				// Validate the mode before announcing or executing anything, so the
				// UI never shows a tool starting only to have it denied afterwards.
				if !tools.Allowed(session.Mode, call.Function.Name) {
					reason := tools.ModeBlockReason(session.Mode, call.Function.Name)
					call.Result = reason
					call.Status = "blocked"
					logx.Warn("tool blocked by mode", "session", session.ID, "round", totalRounds,
						"tool", call.Function.Name, "mode", session.Mode)
					if err := stream.send(chatEvent{
						Type: "tool_blocked", Round: totalRounds, ID: call.ID, Name: call.Function.Name,
						Message: reason, Status: "blocked",
					}); err != nil {
						if id, _ := s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls); id > 0 {
							lastAssistantID = id
						}
						saveTurnTiming()
						return
					}
					if verdict := guard.observe(*call, tools.Result{Content: reason, IsError: true}); verdict.Action != guardOK {
						if verdict.Action == guardAbort {
							if abortReason == "" {
								abortReason, abortMessage = verdict.Reason, verdict.Note
							}
						} else {
							guardNote = verdict.Note
						}
					}
					continue
				}
				if err := stream.send(chatEvent{
					Type: "tool_start", Round: totalRounds, ID: call.ID, Name: call.Function.Name,
					Arguments: call.Function.Arguments, Status: "running",
				}); err != nil {
					if id, _ := s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls); id > 0 {
						lastAssistantID = id
					}
					saveTurnTiming()
					return
				}
				started := time.Now()
				var toolResult tools.Result
				if call.Function.Name == "ask_user" && len(askedBatches) > 0 {
					// One batch per round: the UI starts the next turn as soon as
					// the user answers, so a second pending batch would be stranded.
					toolResult = tools.Result{
						Content: "ask_user: only one question batch per round is allowed; merge these questions into a single call.",
						IsError: true,
					}
				} else {
					toolResult = mgr.ExecuteCall(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments), call.ID)
				}
				elapsed := time.Since(started).Milliseconds()
				ledger.observe(*call, toolResult)
				if toolResult.IsError {
					logx.Warn("tool failed", "session", session.ID, "round", totalRounds, "tool", call.Function.Name,
						"duration_ms", elapsed, "error", logx.Preview(toolResult.Content, 200))
				} else {
					logx.Debug("tool finished", "session", session.ID, "round", totalRounds, "tool", call.Function.Name,
						"duration_ms", elapsed, "result_chars", len(toolResult.Content))
				}
				if verdict := guard.observe(*call, toolResult); verdict.Action != guardOK {
					if verdict.Action == guardAbort {
						if abortReason == "" {
							abortReason, abortMessage = verdict.Reason, verdict.Note
						}
					} else {
						guardNote = verdict.Note
					}
				}
				call.Result = toolResult.Content
				call.Status = "done"
				if toolResult.IsError {
					call.Status = "error"
				}
				call.DurationMS = elapsed
				if err := stream.send(chatEvent{
					Type: "tool_end", Round: totalRounds, ID: call.ID, Name: call.Function.Name,
					Result: toolResult.Content, IsError: toolResult.IsError, DurationMS: elapsed,
					Status: call.Status, Files: toolResult.Files,
				}); err != nil {
					if id, _ := s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls); id > 0 {
						lastAssistantID = id
					}
					saveTurnTiming()
					return
				}
				if toolResult.ChecklistChanged {
					if items, err := s.store.ListChecklist(session.ID); err == nil {
						stream.send(chatEvent{Type: "checklist", Checklist: &items})
					}
				}
				if toolResult.QuestionBatch != nil {
					askedBatches = append(askedBatches, toolResult.QuestionBatch)
				}
			}
			if len(calls) > 0 {
				toolRounds++
			}

			if id, err := s.persistToolCalls(session.ID, result.Reasoning, result.Content, calls); err != nil {
				stream.fail("Could not save tool results: " + err.Error())
				saveTurnTiming()
				return
			} else if id > 0 {
				lastAssistantID = id
			}
			if ctx.Err() != nil {
				cancelTurn("", "")
				return
			}

			// ask_user is terminal: the batch is already persisted, so the
			// browser can render it (or reload it later) and the user answers
			// in the chat. Asking the model for another round would only make
			// it talk to itself while the user is away.
			if len(askedBatches) > 0 {
				for _, batch := range askedBatches {
					_ = stream.send(chatEvent{Type: "questions", QuestionBatch: batch})
				}
				logx.Info("turn paused for user answers", "session", session.ID,
					"round", totalRounds, "batches", len(askedBatches))
				finish(false)
				return
			}

			// Stop conditions that are cheaper to evaluate after the round has
			// been persisted, so no tool result is ever lost.
			if abortReason != "" {
				// In PLAN, a pathological loop should still produce a plan instead
				// of ending with no answer. Disable tools and force the final
				// response on the next round. BUILD keeps the existing pause.
				if session.Mode == "PLAN" && !finalizing && totalRounds < turnLimit {
					logx.Warn("plan guard forcing final answer", "session", session.ID,
						"reason", abortReason, "round", totalRounds)
					finalizing = true
					finalAttempts = 0
					finalReason = "the agent stopped repeating tool calls"
					abortReason, abortMessage = "", ""
					guardNote = ""
					continue
				}
				logx.Warn("agent guard stopped the turn", "session", session.ID,
					"reason", abortReason, "round", totalRounds)
				stream.send(chatEvent{
					Type: "limit", Round: totalRounds, Limit: turnLimit,
					Segment: segment, SegmentSize: segmentSize,
					Reason: abortReason, Message: abortMessage,
				})
				finish(false)
				return
			}
			if turnCostLimit > 0 && turnCost >= turnCostLimit {
				logx.Warn("turn cost budget reached", "session", session.ID,
					"cost", turnCost, "limit", turnCostLimit, "round", totalRounds)
				stream.send(chatEvent{
					Type: "limit", Round: totalRounds, Limit: turnLimit,
					Segment: segment, SegmentSize: segmentSize,
					Reason:  "cost_budget",
					Message: fmt.Sprintf("Paused after $%.4f (turn budget $%.4f). Send another message to continue.", turnCost, turnCostLimit),
				})
				finish(false)
				return
			}
		}

		// The segment finished. Either the turn budget is spent or the model was
		// still working, in which case we either pause or continue automatically.
		if ctx.Err() != nil {
			cancelTurn("", "")
			return
		}
		if totalRounds >= turnLimit {
			break
		}
		if !autoContinue {
			logx.Info("turn checkpoint reached", "session", session.ID,
				"rounds", totalRounds, "segment", segment)
			stream.send(chatEvent{
				Type: "checkpoint", Round: totalRounds, Limit: turnLimit,
				Segment: segment, SegmentSize: segmentSize, Reason: "paused",
				Message: fmt.Sprintf("Checkpoint at %d rounds. Continue when ready.", totalRounds),
			})
			finish(false)
			return
		}
		segment++
		continueNote = continuationNote
		logx.Info("turn segment continued", "session", session.ID,
			"rounds", totalRounds, "segment", segment)
		stream.send(chatEvent{
			Type: "checkpoint", Round: totalRounds, Limit: turnLimit,
			Segment: segment, SegmentSize: segmentSize, NextRound: totalRounds + 1,
			Reason: "auto", Message: fmt.Sprintf("Checkpoint at %d rounds — continuing.", totalRounds),
		})
	}

	logx.Warn("turn round budget reached", "session", session.ID, "limit", turnLimit)
	stream.send(chatEvent{
		Type: "limit", Round: totalRounds, Limit: turnLimit,
		Segment: segment, SegmentSize: segmentSize, Reason: "turn_budget",
		Message: fmt.Sprintf("Paused after %d rounds (turn budget). Send another message or press Continue to keep going.", turnLimit),
	})
	finish(false)
}

func (s *Server) persistPartial(sessionID int64, reasoning, content string) int64 {
	if strings.TrimSpace(reasoning) == "" && strings.TrimSpace(content) == "" {
		return 0
	}
	msg, err := s.store.AddMessage(sessionID, "assistant", reasoning, content, "")
	if err != nil {
		logx.Error("persist partial response failed", "session", sessionID, "error", err)
		return 0
	}
	return msg.ID
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

func (s *Server) sendDone(stream *runPublisher, sessionID int64, reasoningOnly bool) {
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

// looksLikeContextOverflow recognizes provider errors caused by an oversized
// request so the UI can point the user at /compact instead of a raw API error.
func looksLikeContextOverflow(msg string) bool {
	lower := strings.ToLower(msg)
	for _, needle := range []string{
		"context length", "context_length", "maximum context",
		"too many tokens", "token limit", "context window",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// effectiveHistoryResultLimit keeps PLAN research reads intact. The default
// 4,000-char result cap clips ~100-line reads in half, which makes the model
// re-read windows and wastes the research budget. A configured 0 still disables
// the cap entirely.
func effectiveHistoryResultLimit(mode string, configured int) int {
	if mode == "PLAN" && configured > 0 && configured < planMinResultChars {
		return planMinResultChars
	}
	return configured
}

// buildSystemPrompt assembles the stable instruction prefix. It intentionally
// excludes volatile state (checklist, repository outline) so the provider's
// prompt cache can reuse this prefix across rounds. Volatile state is injected
// as a trailing ephemeral message by buildStateMessage.
func buildSystemPrompt(project store.Project, session store.Session, segmentSize, turnLimit, planRounds, planToolCalls int) string {
	var b strings.Builder
	b.WriteString("You are Forge, an expert autonomous coding agent operating on a local project through a web UI.\n\n")
	fmt.Fprintf(&b, "Project: %s\nWorking directory: %s\n", project.Name, project.Path)
	fmt.Fprintf(&b, "Mode: %s\n", session.Mode)
	if session.Mode == "PLAN" {
		b.WriteString("PLAN mode is READ-ONLY. You may inspect files, search, list directories, run git diff, update the checklist, and ask the user clarifying questions. You cannot write files or run shell commands.\n")
		b.WriteString("Respond in the language the user is speaking (IMPORTANT). All code and technical content must be in English.\n")
		b.WriteString("Adapt your response to the conversation: discuss trade-offs and findings naturally during exploration, and output a structured implementation plan only when ready for execution. Always put your response in normal content, never only in reasoning. Never claim that files were changed or commands were run in PLAN mode.\n")
		b.WriteString("Call ask_user when a design, engineering, or data decision is ambiguous or missing, and the user can clarify it—especially when guessing would risk wrong architecture, breaking contracts, or misinterpreting data/schemas. Present concise options plus a custom answer when helpful, and end the turn so the user can respond. Do not call ask_user for implementation details you can resolve from the codebase or decide autonomously, nor for cosmetic preferences.\n")
		b.WriteString("If the latest user message begins with \"Answers to your questions:\", the user has resolved your questions: continue from those answers without repeating the questions or redoing completed research.\n")
	} else {
		b.WriteString("BUILD mode allows reading, writing, patching, and running shell commands. Make the requested changes directly, then verify them. Action-first rule in BUILD: When a plan already exists in the conversation, do NOT re-explore the codebase. Read only the exact line ranges cited in the plan, apply patch_file/write_file within your first 1–2 rounds, and verify immediately.\n")
		b.WriteString("Anti-loop rule: Never perform more than 2 consecutive read/search rounds without making an edit or running a verification command.\n")
		b.WriteString("Use ask_user only when you are genuinely blocked by a decision only the user can make; do not ask for cosmetic preferences.\n")
		b.WriteString("Respond in the language the user is speaking (IMPORTANT). Implement changes in English by default unless the user specifies otherwise.\n")
	}
	allowed := make([]string, 0, 10)
	for _, spec := range tools.Specs(session.Mode) {
		allowed = append(allowed, spec.Function.Name)
	}
	fmt.Fprintf(&b, "\nAllowed tools in %s mode: %s\n", session.Mode, strings.Join(allowed, ", "))
	b.WriteString("Never call a tool outside this list. Disallowed calls are blocked before execution and reported back to you.\n")
	b.WriteString("Tool calls must use the native tool-calling interface. Never write <tool_call>, <function=...>, XML, or JSON tool calls in the response text; they are not executed.\n")
	b.WriteString("Batch independent tool calls in the same response. One round can contain several reads or searches; use that to conserve the budget.\n")
	if session.Mode == "PLAN" {
		fmt.Fprintf(&b, `
PLAN research budget:
- This turn allows %d tool rounds and %d tool calls. The harness stops after the round that reaches either limit and requires the final plan.
- Every response counts as one round even when it contains several tool calls. Batch independent reads and searches in the same response.
- Stop as soon as you can name the files, the symbols, and the exact changes. Do not try to inspect every line of the project.
- The research ledger in the current-state message lists what you already read and searched this turn.
- If the budget is reached, answer with the best plan you can and state any remaining uncertainty in one short line.
`, planRounds, planToolCalls)
	} else {
		fmt.Fprintf(&b, `
Step budget:
- This turn allows up to %d model rounds and is checkpointed every %d rounds; it continues automatically when needed.
- Every response counts as one round even when it contains several tool calls. Batch independent reads, searches, and edits in the same response to conserve the budget.
- Do not stop early just because a task is large. When a checkpoint approaches, update the checklist and leave a clear handoff.
`, turnLimit, segmentSize)
	}
	b.WriteString(`
Operating rules:
- Initialize or update the checklist with update_checklist in your first batch alongside your initial file reads: add the task with action=add (or action=set for a multi-step plan). Keep the list current as you work (IMPORTANT).
- SYMBOL-FIRST & EXPLORATION ORDER (strict): Always follow this hierarchy and never read or guess file contents blindly: (1) symbol_lookup first whenever you know or can guess a function, method, struct, class, or variable name — it returns the exact file and line range in one call; (2) the compact Repository outline in the current-state message for project structure and breadth; (3) list_directory; (4) grep_search last, strictly for free text, error strings, config keys, or regular expressions. If symbol_lookup returns no match, fall back to grep_search once and move on.
- read_file requires explicit start_line and end_line and returns at most 200 lines. Choose the narrowest window around the symbol you need; use the continuation hint to page through larger sections. Requests over 200 lines are capped automatically.
- Every read_file line is prefixed with its real line number (for example "142 | func Save() {"). Use those numbers to choose precise edit locations.
- The "NNN | " prefix is display-only. Never include it in patch_file search/replace text; copy the code without the prefix.
- Do not re-read a file or range you already inspected unless it changed; use the conversation (and, in PLAN, the research ledger) instead of repeating reads.
- Stay strictly inside the project directory. Never attempt to access paths outside it.
- SHELL PATHS: every bash call starts in the project root in a fresh shell, so "cd" never persists between calls. Do not probe relative paths. If a command must run in another directory, put a single "cd" in the same invocation using an absolute, quoted literal path, for example: cd "/abs/path" && go test ./... — otherwise stay in the project root and pass paths as quoted literals.
- Read relevant files before editing. Prefer patch_file for small, surgical changes and write_file for new or fully rewritten files.
- Read both the surrounding code and existing patterns before making changes; match local style.
- The Repository outline and symbol index are refreshed when Forge writes files. After bash commands or external edits, verify with symbol_lookup or grep_search before relying on cached line numbers.
- Run focused tests or builds when appropriate. Never claim a test passed unless you ran it.
- Tool output and reasoning are rendered as collapsed accordions in the UI, so never repeat tool output in your answer.
`)
	b.WriteString(`
Response style (strict):
- Do not narrate tool use ("Now I will…", "Let me check…"); the UI already shows every call and result.
- Only the final message (the one without tool calls) is written for the user.
- No preamble, no praise, no restating code, no long explanations.
`)
	if session.Mode == "PLAN" {
		b.WriteString(`- PLAN mode response behavior (adaptive): Markdown text only, no tool-call syntax. Adapt your response to the conversation stage:
1. **Exploratory / Architectural Discussion** (when the user is asking questions, debating trade-offs, investigating a bug without requesting a fix yet, or when critical design choices remain unresolved): respond naturally as a senior architect. Present your findings, trade-offs, and concrete options clearly using concise prose or bullets. If the user must choose a direction before the work can continue, call ask_user as the final action of that response with concrete options instead of ending with a list of questions in prose.
2. **Actionable Implementation Plan** (when the user requests a concrete change/plan or confirms the design direction): output a structured plan for human architectural review before execution. Write NO raw code blocks, NO SQL queries, and NO verbose inline signatures: describe changes in prose and cite line numbers only as anchors. Use an Implementation Plan like this:

## Diagnosis & Approach
A concise 2-4 sentence summary explaining the root problem or current limitation and the high-level architectural strategy to solve it.

## Key Decisions
- Short, punchy bullet points explaining trade-offs, edge cases handled, and why existing patterns are reused or avoided.

## Implementation Plan
Group changes strictly by file path (1-2 clear sentences per item describing what changes and why):
- **path/to/file.go (lines X-Y)**: Description of the structural change or new functions.
- **path/to/new_file.go (new)**: Description of the module's responsibility.

Do not add an "Open Questions" section: ask blocking questions with ask_user before writing the plan, or state a short assumption inline if you cannot ask.

End the Actionable Implementation Plan with this exact line:
Ready to implement — switch to BUILD to apply this plan.

Rules for both modes: never repeat raw tool output or file dumps; never write vague summaries ("update the UI") — always cite exact files, symbols, and line ranges discovered.
`)
	} else {
		b.WriteString("- BUILD final message: concise, factual summary (5-7 lines max) covering: (1) exact files created/modified and what changed; (2) verification run and its real outcome (never claim success if a test/build failed or was skipped); (3) any blockers, partial progress, or next steps. No headings, no unrequested code, no filler.\n")
	}
	return strings.TrimSpace(b.String())
}

// buildStateMessage renders the volatile session state. It is injected as the
// final ephemeral message of each request (never persisted), so changing it
// does not invalidate the cached prompt prefix.
func buildStateMessage(checklist []store.ChecklistItem, outline string) string {
	var b strings.Builder
	b.WriteString("[Current session state — refreshed automatically; this is context, not a new request]\n")
	if len(checklist) > 0 {
		b.WriteString("Checklist:\n")
		for _, it := range checklist {
			mark := "[ ]"
			if it.IsDone {
				mark = "[x]"
			}
			fmt.Fprintf(&b, "%s #%d %s\n", mark, it.ID, it.TaskText)
		}
	}
	if strings.TrimSpace(outline) != "" {
		b.WriteString("Repository outline (compact; line numbers refer to the current files):\n")
		b.WriteString(strings.TrimSpace(outline))
		b.WriteString("\nEnd of repository outline.\n")
	} else {
		b.WriteString("Repository outline: unavailable. Use symbol_lookup for a known declaration name, or list_directory and grep_search to locate files.\n")
	}
	return strings.TrimSpace(b.String())
}

// buildFinalPlanNote is the last ephemeral message of the forced PLAN answer.
// It is deliberately blunt: after this point the only acceptable output is the
// plan in normal response content.
func buildFinalPlanNote(reason string, planRounds, planToolCalls, usedRounds, usedCalls, attempt int, noTools bool) string {
	var b strings.Builder
	b.WriteString("[FINAL ANSWER REQUIRED — PLAN research stopped]\n")
	if reason == "" {
		reason = "research budget exhausted"
	}
	fmt.Fprintf(&b, "Reason: %s. Used %d/%d tool rounds and %d/%d tool calls.\n",
		reason, usedRounds, planRounds, usedCalls, planToolCalls)
	if noTools {
		b.WriteString("No tools are available in this response. Any tool call is ignored.\n")
	} else {
		b.WriteString("Tools are disabled for this response and any tool call is ignored. Do not call any tool.\n")
	}
	b.WriteString("Do not write tool calls in the response text: no <tool_call>, <function=...>, XML, or JSON tool-call syntax. They are not executed.\n")
	b.WriteString("The research phase is over. Use only the information already in the conversation.\n")
	if attempt > 1 {
		b.WriteString("Your previous final response was not a usable plan (empty, reasoning-only, or a tool call written as text). This time write the plan in Markdown only.\n")
	}
	b.WriteString(`Write the final answer now, in normal response content, with the full PLAN deliverable, structured for human architectural review:
- "## Diagnosis & Approach": 2-4 sentences on the root problem and the high-level strategy.
- "## Key Decisions": short bullets on trade-offs, edge cases, and patterns reused or avoided.
- "## Implementation Plan": items grouped strictly by file path, each 1-2 sentences on what changes and why, citing line numbers only as anchors, e.g. "- **path/to/file.go (lines X-Y)**: change." and "- **path/to/new_file.go (new)**: responsibility.".
- Do not add an "Open Questions" section: tools are disabled here, so state any remaining assumption in one short line.
- Then the line exactly: "Ready to implement — switch to BUILD to apply this plan."
No raw code blocks, no SQL queries, no verbose inline signatures, no raw tool output.
Do not try to research further; if something is missing, state the assumption instead.`)
	return b.String()
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
