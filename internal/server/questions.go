package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"forge/internal/store"
)

func (s *Server) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	batches, err := s.store.ListQuestionBatches(id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"question_batches": batches})
}

func (s *Server) handleAnswerQuestions(w http.ResponseWriter, r *http.Request) {
	sessionID, err := pathID(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	batchID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("qid")), 10, 64)
	if err != nil || batchID <= 0 {
		apiError(w, http.StatusBadRequest, "invalid question batch id")
		return
	}
	var body struct {
		Answers []struct {
			QuestionID int64   `json:"question_id"`
			OptionIDs  []int64 `json:"option_ids"`
			Text       string  `json:"text"`
		} `json:"answers"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Answers) == 0 {
		apiError(w, http.StatusBadRequest, "answers are required")
		return
	}
	answers := make([]store.QuestionAnswerInput, 0, len(body.Answers))
	for _, a := range body.Answers {
		answers = append(answers, store.QuestionAnswerInput{
			QuestionID: a.QuestionID,
			OptionIDs:  a.OptionIDs,
			Text:       a.Text,
		})
	}
	batch, err := s.store.AnswerQuestionBatch(sessionID, batchID, answers)
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "question batch not found")
		return
	case errors.Is(err, store.ErrAlreadyAnswered):
		apiError(w, http.StatusConflict, "these questions have already been answered")
		return
	case err != nil:
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch":   batch,
		"summary": questionAnswerSummary(batch),
	})
}

// questionAnswerSummary builds the canonical user message that carries the
// answers back to the model. The leading line is also the PLAN prompt's signal
// that the clarifying-questions phase is complete.
func questionAnswerSummary(batch store.QuestionBatch) string {
	var b strings.Builder
	b.WriteString("Answers to your questions:\n")
	for i, q := range batch.Questions {
		fmt.Fprintf(&b, "%d. %s\n", i+1, q.Text)
		parts := make([]string, 0, len(q.AnswerOptionIDs)+1)
		for _, id := range q.AnswerOptionIDs {
			for _, opt := range q.Options {
				if opt.ID == id {
					if label := strings.TrimSpace(opt.Label); label != "" {
						parts = append(parts, label)
					}
					break
				}
			}
		}
		if text := strings.TrimSpace(q.AnswerText); text != "" {
			parts = append(parts, text)
		}
		answer := strings.Join(parts, "; ")
		if answer == "" {
			answer = "(no answer)"
		}
		fmt.Fprintf(&b, "   Answer: %s\n", answer)
	}
	b.WriteString("\nThe user answered the clarifying questions above. Continue from these answers without repeating the questions.")
	return strings.TrimSpace(b.String())
}
