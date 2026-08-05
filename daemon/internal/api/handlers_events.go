package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

const maxLongPoll = 55 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	timeout := maxLongPoll
	if t := r.URL.Query().Get("timeout"); t != "" {
		if secs, err := strconv.Atoi(t); err == nil && secs >= 0 && secs <= 55 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	// cursor==0: fast-forward the client to now with an empty list; they
	// reconcile via /pack/current (API.md).
	if cursor == 0 {
		latest, err := s.store.LatestSeq(r.Context())
		if err != nil {
			writeError(w, codeInternal, "events read failed")
			return
		}
		writeEvents(w, nil, latest)
		return
	}

	deadline := time.Now().Add(timeout)
	for {
		events, err := s.store.EventsAfter(r.Context(), cursor, 200)
		if err != nil {
			writeError(w, codeInternal, "events read failed")
			return
		}
		if len(events) > 0 {
			writeEvents(w, events, events[len(events)-1].Seq)
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeEvents(w, nil, cursor)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), remaining)
		s.store.Wait(ctx, cursor)
		cancel()
		if r.Context().Err() != nil {
			return
		}
	}
}

type eventView struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

func writeEvents(w http.ResponseWriter, events []store.Event, cursor int64) {
	views := make([]eventView, 0, len(events))
	for _, e := range events {
		views = append(views, eventView{
			Seq:       e.Seq,
			Type:      e.Type,
			CreatedAt: e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			Payload:   json.RawMessage(e.Payload),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": views, "cursor": cursor})
}
