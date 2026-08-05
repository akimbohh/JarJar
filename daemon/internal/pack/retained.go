package pack

import "sync"

// retainedSet is the concurrency-safe set of blob hashes referenced by retained
// manifests. The blob download endpoint checks it before serving.
type retainedSet struct {
	mu  sync.RWMutex
	set map[string]bool
}

func newRetainedSet() retainedSet { return retainedSet{set: map[string]bool{}} }

func (r *retainedSet) has(sha string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.set[sha]
}

func (r *retainedSet) replace(set map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set = set
}

func (r *retainedSet) snapshot() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool, len(r.set))
	for k := range r.set {
		out[k] = true
	}
	return out
}
