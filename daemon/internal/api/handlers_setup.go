package api

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/akimbohh/jarjar/daemon/internal/bootstrap"
)

// handleGetSetup reports whether the server is configured yet, plus live setup
// progress. The app polls this to choose between the onboarding wizard and the
// dashboard and to render the step checklist.
func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	if s.setup == nil {
		writeError(w, codeInternal, "setup is not available on this server")
		return
	}
	writeJSON(w, http.StatusOK, s.setup.Status(r.Context()))
}

// handleStartSetup kicks off the one-time, app-driven setup: the AI designs the
// modpack, the server is provisioned and booted, version 1 is published.
func (s *Server) handleStartSetup(w http.ResponseWriter, r *http.Request) {
	if s.setup == nil {
		writeError(w, codeInternal, "setup is not available on this server")
		return
	}
	var req bootstrap.SetupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.setup.Start(r.Context(), req); err != nil {
		writeError(w, codeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// modView is one entry of the dashboard mods list.
type modView struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Side string `json:"side"`
}

// handlePackMods returns the mods in the current pack version, derived from the
// manifest. Names come from the jar filename (no registry calls), which is
// enough for the app's mod browser.
func (s *Server) handlePackMods(w http.ResponseWriter, r *http.Request) {
	v, ok, err := s.store.CurrentVersion(r.Context())
	if err != nil {
		writeError(w, codeInternal, "pack read failed")
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"mods": []modView{}})
		return
	}
	buf, err := s.pack.ManifestBytes(r.Context(), v.Number)
	if err != nil {
		writeError(w, codeInternal, "manifest read failed")
		return
	}
	var m struct {
		Files []struct {
			Path string `json:"path"`
			Side string `json:"side"`
			Kind string `json:"kind"`
		} `json:"files"`
	}
	if err := json.Unmarshal(buf, &m); err != nil {
		writeError(w, codeInternal, "manifest parse failed")
		return
	}
	mods := make([]modView, 0)
	for _, f := range m.Files {
		if f.Kind != "mod" {
			continue
		}
		mods = append(mods, modView{Name: modName(f.Path), Path: f.Path, Side: f.Side})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mods": mods})
}

// modName turns "mods/sodium-fabric-0.5.3.jar" into a readable "sodium-fabric".
func modName(p string) string {
	base := path.Base(p)
	base = strings.TrimSuffix(base, ".jar")
	return base
}
