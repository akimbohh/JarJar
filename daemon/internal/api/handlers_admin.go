package api

import (
	"errors"
	"net/http"

	"github.com/akimbohh/jarjar/daemon/internal/id"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

type createInviteBody struct {
	Role string `json:"role"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	var body createInviteBody
	if !decodeJSON(w, r, &body) {
		return
	}
	role := body.Role
	if role == "" {
		role = store.RolePlayer
	}
	if role != store.RolePlayer && role != store.RoleAdmin {
		writeError(w, codeInvalidRequest, "role must be player or admin")
		return
	}
	inv, err := s.store.CreateInvite(r.Context(), id.NewInviteCode(), role)
	if err != nil {
		writeError(w, codeInternal, "create invite failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite_code": inv.Code})
}

func (s *Server) handleListPlayers(w http.ResponseWriter, r *http.Request) {
	players, err := s.store.ListPlayers(r.Context())
	if err != nil {
		writeError(w, codeInternal, "list failed")
		return
	}
	type pv struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]pv, 0, len(players))
	for _, p := range players {
		out = append(out, pv{p.ID, p.Name, p.Role, p.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})
	}
	writeJSON(w, http.StatusOK, map[string]any{"players": out})
}

func (s *Server) handleDeletePlayer(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeletePlayer(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, codeNotFound, "no such player")
		return
	} else if errors.Is(err, store.ErrConflict) {
		writeError(w, codeConflict, err.Error())
		return
	} else if err != nil {
		writeError(w, codeInternal, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.RequestByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, codeNotFound, "no such request")
		return
	} else if err != nil {
		writeError(w, codeInternal, "lookup failed")
		return
	}
	if req.Status != store.StatusAwaitingApproval {
		writeError(w, codeConflict, "request is not awaiting approval")
		return
	}
	updated, err := s.store.SetStatus(r.Context(), req.ID, store.StatusMaterializing)
	if err != nil {
		writeError(w, codeInternal, "update failed")
		return
	}
	s.emitRequestUpdated(r.Context(), updated)
	s.jobs.Wake()
	writeJSON(w, http.StatusOK, viewOf(updated))
}

type rejectBody struct {
	Reason string `json:"reason"`
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var body rejectBody
	if !decodeJSON(w, r, &body) {
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
	if req.Status != store.StatusAwaitingApproval {
		writeError(w, codeConflict, "request is not awaiting approval")
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "rejected by admin"
	}
	updated, err := s.store.SetStatus(r.Context(), req.ID, store.StatusRejected, store.WithError(reason))
	if err != nil {
		writeError(w, codeInternal, "update failed")
		return
	}
	s.emitRequestUpdated(r.Context(), updated)
	writeJSON(w, http.StatusOK, viewOf(updated))
}

type rollbackBody struct {
	ToVersion int `json:"to_version"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var body rollbackBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ToVersion < 1 {
		writeError(w, codeInvalidRequest, "to_version must be a positive integer")
		return
	}
	if _, err := s.store.VersionByNumber(r.Context(), body.ToVersion); errors.Is(err, store.ErrNotFound) {
		writeError(w, codeInvalidRequest, "no such version")
		return
	}
	if active, ok, _ := s.store.ActiveRequest(r.Context()); ok {
		writeError(w, codeConflict, "another request is in progress: "+active.ID)
		return
	}
	admin := playerFrom(r.Context())
	reqID, err := s.jobs.EnqueueRollback(r.Context(), body.ToVersion, admin.ID)
	if err != nil {
		writeError(w, codeInternal, "rollback enqueue failed")
		return
	}
	s.jobs.Wake()
	writeJSON(w, http.StatusAccepted, map[string]any{"request_id": reqID})
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.jobs.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"queue":           snap.Queue,
		"current_job":     snap.CurrentJob,
		"mc_server":       s.mc.State(r.Context()),
		"disk_free_bytes": s.diskFree(s.cfg.Server.DataDir),
	})
}
