package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/store"
)

type ctxKey int

const playerKey ctxKey = 1

func playerFrom(ctx context.Context) store.Player {
	p, _ := ctx.Value(playerKey).(store.Player)
	return p
}

// auth wraps a handler requiring at least the given role, applying the rate
// limiter. Admin satisfies the player requirement.
func (s *Server) auth(minRole string, h http.HandlerFunc) http.Handler {
	return s.authWith(minRole, true, h)
}

// authNoLimit is like auth but exempt from the rate limiter (blob downloads).
func (s *Server) authNoLimit(minRole string, h http.HandlerFunc) http.Handler {
	return s.authWith(minRole, false, h)
}

func (s *Server) authWith(minRole string, limit bool, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, codeUnauthorized, "missing bearer token")
			return
		}
		p, err := s.store.PlayerByToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, codeUnauthorized, "invalid token")
			return
		} else if err != nil {
			writeError(w, codeInternal, "auth lookup failed")
			return
		}
		if minRole == store.RoleAdmin && p.Role != store.RoleAdmin {
			writeError(w, codeForbidden, "admin role required")
			return
		}
		if limit && !s.limiter.allow(p.ID) {
			w.Header().Set("Retry-After", "60")
			writeError(w, codeRateLimited, "rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), playerKey, p)
		h(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// withGlobal applies headers that every response carries.
func (s *Server) withGlobal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-JarJar-Min-Client", MinClient)
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a per-key fixed-window limiter (blob downloads are exempted by
// the handlers that skip it — see handleBlob).
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string]*windowCount{}}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	wc := rl.hits[key]
	if wc == nil || now.Sub(wc.start) > rl.window {
		rl.hits[key] = &windowCount{start: now, n: 1}
		return true
	}
	if wc.n >= rl.limit {
		return false
	}
	wc.n++
	return true
}
