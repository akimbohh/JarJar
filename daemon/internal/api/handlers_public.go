package api

import (
	"errors"
	"net/http"

	"github.com/akimbohh/jarjar/daemon/internal/id"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"status":  "ok",
		"version": Version,
	}
	if v, ok, _ := s.store.CurrentVersion(r.Context()); ok {
		resp["pack_version"] = v.Number
	} else {
		resp["pack_version"] = 0
	}
	resp["mc_server"] = s.mc.State(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

type joinRequest struct {
	InviteCode string `json:"invite_code"`
	PlayerName string `json:"player_name"`
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !id.ValidInviteCode(req.InviteCode) {
		writeError(w, codeInvalidRequest, "invalid invite code format")
		return
	}
	if !id.ValidPlayerName(req.PlayerName) {
		writeError(w, codeInvalidRequest, "player_name must be 1-32 chars of [A-Za-z0-9_-]")
		return
	}
	token := id.NewToken()
	p, err := s.store.RedeemInvite(r.Context(), req.InviteCode, req.PlayerName, token)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, codeInvalidRequest, err.Error())
		return
	} else if err != nil {
		writeError(w, codeInternal, "join failed")
		return
	}

	serverName := ""
	if meta, ok, _ := s.pack.Meta(r.Context()); ok {
		serverName = meta.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":       token,
		"player_id":   p.ID,
		"role":        p.Role,
		"server_name": serverName,
	})
}
