package store

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// AppendEvent inserts an event and returns its assigned seq. It also wakes any
// long-poll waiters registered via Notify/Wait.
func (s *Store) AppendEvent(ctx context.Context, typ string, payload []byte) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (type, payload, created_at) VALUES (?, ?, ?)`,
		typ, string(payload), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.hub.broadcast(seq)
	return seq, nil
}

// EventsAfter returns events with seq > cursor, oldest-first, up to limit.
func (s *Store) EventsAfter(ctx context.Context, cursor int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, payload, created_at FROM events WHERE seq > ? ORDER BY seq LIMIT ?`,
		cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload, created string
		if err := rows.Scan(&e.Seq, &e.Type, &payload, &created); err != nil {
			return nil, err
		}
		e.Payload = []byte(payload)
		e.CreatedAt = parseRFC(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestSeq returns the highest event seq (0 if none).
func (s *Store) LatestSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq); err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

// PruneEvents deletes events older than the cutoff. Returns rows removed.
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, olderThan.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- long-poll wakeup hub ----
//
// The worker process writes events to the same DB but cannot signal the serve
// process's in-memory hub. Serve therefore also polls the DB on a short ticker
// (see internal/api). Within the serve process, AppendEvent broadcasts
// immediately so same-process writers wake waiters with no latency.

type hub struct {
	mu      sync.Mutex
	waiters map[chan int64]struct{}
}

func newHub() *hub { return &hub{waiters: map[chan int64]struct{}{}} }

func (h *hub) broadcast(seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.waiters {
		select {
		case ch <- seq:
		default:
		}
	}
}

// Wait blocks until an event newer than cursor is available or ctx is done.
// It returns immediately if the store already has such an event. A caller that
// wakes should re-query EventsAfter (the seq delivered is only a hint).
func (s *Store) Wait(ctx context.Context, cursor int64) {
	// Fast path: already have newer events.
	if latest, _ := s.LatestSeq(ctx); latest > cursor {
		return
	}
	ch := make(chan int64, 1)
	s.hub.mu.Lock()
	s.hub.waiters[ch] = struct{}{}
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		delete(s.hub.waiters, ch)
		s.hub.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
	case <-ch:
	}
}

// Poke lets an external trigger (e.g. the serve-side DB poller that watches for
// worker-written events) wake all waiters.
func (s *Store) Poke(seq int64) { s.hub.broadcast(seq) }
