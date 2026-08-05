package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// zeroTime disables Last-Modified in ServeContent (blobs are immutable and
// addressed by content hash, so the ETag/Range behavior is what matters).
var zeroTime = time.Time{}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) handlePackCurrent(w http.ResponseWriter, r *http.Request) {
	meta, ok, err := s.pack.Meta(r.Context())
	if err != nil {
		writeError(w, codeInternal, "pack read failed")
		return
	}
	resp := map[string]any{"version": 0, "summary": "", "pack": meta}
	if v, has, _ := s.store.CurrentVersion(r.Context()); has {
		resp["version"] = v.Number
		resp["summary"] = v.Summary
	}
	if !ok {
		// No import yet: still return a well-formed shape.
		resp["pack"] = PackMeta{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, codeInvalidRequest, "version must be a positive integer")
		return
	}
	buf, err := s.pack.ManifestBytes(r.Context(), n)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, codeNotFound, "no such version")
		return
	} else if err != nil {
		writeError(w, codeInternal, "manifest read failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "immutable, max-age=31536000")
	w.WriteHeader(http.StatusOK)
	w.Write(buf)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRe.MatchString(sha) {
		writeError(w, codeInvalidRequest, "sha must be 64 lowercase hex chars")
		return
	}
	if !s.pack.BlobRetained(sha) {
		writeError(w, codeNotFound, "blob not found")
		return
	}
	rs, size, err := s.pack.OpenBlob(sha)
	if err != nil {
		writeError(w, codeNotFound, "blob not found")
		return
	}
	defer rs.Close()
	_ = size // Content-Length is set by ServeContent via Seek.
	w.Header().Set("Content-Type", "application/octet-stream")
	// http.ServeContent handles Range/HEAD/If-Range and Content-Length for us.
	http.ServeContent(w, r, sha, zeroTime, rs)
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 100)
	before := 0
	if b := r.URL.Query().Get("before"); b != "" {
		before, _ = strconv.Atoi(b)
	}
	versions, err := s.store.ListVersions(r.Context(), limit, before)
	if err != nil {
		writeError(w, codeInternal, "list failed")
		return
	}
	type versionView struct {
		Number    int              `json:"number"`
		CreatedAt string           `json:"created_at"`
		Summary   string           `json:"summary"`
		RequestID *string          `json:"request_id"`
		Changelog []ChangelogEntry `json:"changelog"`
	}
	out := make([]versionView, 0, len(versions))
	for _, v := range versions {
		cl, _ := s.pack.Changelog(r.Context(), v.Number)
		if cl == nil {
			cl = []ChangelogEntry{}
		}
		out = append(out, versionView{
			Number:    v.Number,
			CreatedAt: v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			Summary:   v.Summary,
			RequestID: v.RequestID,
			Changelog: cl,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}
