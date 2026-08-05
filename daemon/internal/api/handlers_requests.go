package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// requestView is the request object returned by all request endpoints (API.md).
type requestView struct {
	ID         string  `json:"id"`
	PlayerID   string  `json:"player_id"`
	PlayerName string  `json:"player_name"`
	Text       string  `json:"text"`
	Status     string  `json:"status"`
	Summary    *string `json:"summary"`
	Question   *string `json:"question"`
	Error      *string `json:"error"`
	Version    *int    `json:"version"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

func viewOf(r store.Request) requestView {
	v := requestView{
		ID:         r.ID,
		PlayerID:   r.PlayerID,
		PlayerName: r.PlayerName,
		Text:       r.Text,
		Status:     r.Status,
		Question:   r.ClarificationQuestion,
		Error:      r.Error,
		Version:    r.Version,
		CreatedAt:  r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:  r.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	// Summary is drawn from the stored plan's summary once planning succeeds.
	if r.PlanJSON != nil {
		var p struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal([]byte(*r.PlanJSON), &p) == nil && p.Summary != "" {
			v.Summary = &p.Summary
		}
	}
	return v
}

type createRequestBody struct {
	Text string `json:"text"`
}

func (s *Server) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	var body createRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if l := len(body.Text); l < 1 || l > 2000 {
		writeError(w, codeInvalidRequest, "text must be 1-2000 characters")
		return
	}
	// One in-flight request globally.
	if active, ok, err := s.store.ActiveRequest(r.Context()); err != nil {
		writeError(w, codeInternal, "queue check failed")
		return
	} else if ok {
		writeError(w, codeConflict, "another request is in progress: "+active.ID)
		return
	}

	p := playerFrom(r.Context())
	req, err := s.store.CreateRequest(r.Context(), p.ID, body.Text)
	if err != nil {
		writeError(w, codeInternal, "create request failed")
		return
	}
	s.emitRequestUpdated(r.Context(), req)
	s.jobs.Wake()
	writeJSON(w, http.StatusCreated, viewOf(req))
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 100)
	before := r.URL.Query().Get("before")
	reqs, err := s.store.ListRequests(r.Context(), limit, before)
	if err != nil {
		writeError(w, codeInternal, "list failed")
		return
	}
	views := make([]requestView, 0, len(reqs))
	for _, req := range reqs {
		views = append(views, viewOf(req))
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": views})
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.RequestByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, codeNotFound, "no such request")
		return
	} else if err != nil {
		writeError(w, codeInternal, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(req))
}

type answerBody struct {
	Text string `json:"text"`
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var body answerBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if l := len(body.Text); l < 1 || l > 2000 {
		writeError(w, codeInvalidRequest, "text must be 1-2000 characters")
		return
	}
	req, err := s.store.RequestByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, codeNotFound, "no such request")
		return
	} else if err != nil {
		writeError(w, codeInternal, "lookup failed")
		return
	}
	p := playerFrom(r.Context())
	if req.PlayerID != p.ID && p.Role != store.RoleAdmin {
		writeError(w, codeForbidden, "only the requester may answer")
		return
	}
	if req.Status != store.StatusAwaitingClarification {
		writeError(w, codeConflict, "request is not awaiting clarification")
		return
	}
	updated, err := s.store.SetStatus(r.Context(), req.ID, store.StatusPlanning,
		store.WithAnswer(body.Text), store.WithQuestion(""))
	if err != nil {
		writeError(w, codeInternal, "update failed")
		return
	}
	// Clear question on the view.
	updated.ClarificationQuestion = nil
	s.emitRequestUpdated(r.Context(), updated)
	s.jobs.Wake()
	writeJSON(w, http.StatusOK, viewOf(updated))
}

func clampLimit(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// emitRequestUpdated writes a request_updated event.
func (s *Server) emitRequestUpdated(ctx context.Context, r store.Request) {
	payload := map[string]any{
		"request_id":  r.ID,
		"status":      r.Status,
		"player_name": r.PlayerName,
		"text":        r.Text,
		"question":    r.ClarificationQuestion,
		"error":       r.Error,
		"version":     r.Version,
	}
	buf, _ := json.Marshal(payload)
	s.store.AppendEvent(ctx, store.EventRequestUpdated, buf)
}
