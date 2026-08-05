package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/id"
)

// CreateRequest inserts a new request in status "queued".
func (s *Store) CreateRequest(ctx context.Context, playerID, text string) (Request, error) {
	now := time.Now().UTC()
	r := Request{
		ID:        id.NewRequest(),
		PlayerID:  playerID,
		Text:      text,
		Status:    StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO requests (id, player_id, text, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.PlayerID, r.Text, r.Status, now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return Request{}, err
	}
	return s.RequestByID(ctx, r.ID)
}

const requestSelect = `
SELECT r.id, r.player_id, p.name, r.text, r.status, r.plan_json,
       r.clarification_question, r.clarification_answer, r.clarification_rounds,
       r.error, r.version, r.created_at, r.updated_at
FROM requests r JOIN players p ON p.id = r.player_id`

func (s *Store) scanRequest(row rowScanner) (Request, error) {
	var r Request
	var plan, cq, ca, errStr sql.NullString
	var ver sql.NullInt64
	var created, updated string
	err := row.Scan(&r.ID, &r.PlayerID, &r.PlayerName, &r.Text, &r.Status,
		&plan, &cq, &ca, &r.ClarificationRounds, &errStr, &ver, &created, &updated)
	if err != nil {
		if err == sql.ErrNoRows {
			return Request{}, ErrNotFound
		}
		return Request{}, err
	}
	r.PlanJSON = nullStr(plan)
	r.ClarificationQuestion = nullStr(cq)
	r.ClarificationAnswer = nullStr(ca)
	r.Error = nullStr(errStr)
	if ver.Valid {
		v := int(ver.Int64)
		r.Version = &v
	}
	r.CreatedAt = parseRFC(created)
	r.UpdatedAt = parseRFC(updated)
	return r, nil
}

func (s *Store) RequestByID(ctx context.Context, reqID string) (Request, error) {
	return s.scanRequest(s.db.QueryRowContext(ctx, requestSelect+` WHERE r.id = ?`, reqID))
}

// ListRequests returns requests newest-first. If before is non-empty, only
// requests created strictly before that request are returned.
func (s *Store) ListRequests(ctx context.Context, limit int, before string) ([]Request, error) {
	q := requestSelect
	args := []any{}
	if before != "" {
		q += ` WHERE r.created_at < (SELECT created_at FROM requests WHERE id = ?)`
		args = append(args, before)
	}
	q += ` ORDER BY r.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := s.scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRequest returns the single non-terminal request, if any. Enforces the
// one-in-flight-request-at-a-time invariant from API.md.
func (s *Store) ActiveRequest(ctx context.Context) (Request, bool, error) {
	r, err := s.scanRequest(s.db.QueryRowContext(ctx, requestSelect+`
		WHERE r.status NOT IN ('published','failed','rejected','infeasible')
		ORDER BY r.created_at LIMIT 1`))
	if err == ErrNotFound {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// NextQueued returns the oldest queued request, if any (for the job runner).
func (s *Store) NextQueued(ctx context.Context) (Request, bool, error) {
	r, err := s.scanRequest(s.db.QueryRowContext(ctx, requestSelect+`
		WHERE r.status = 'queued' ORDER BY r.created_at LIMIT 1`))
	if err == ErrNotFound {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// SetStatus transitions a request, enforcing the legal-transition table. It
// touches updated_at and (optionally) error/version/plan fields via opts.
func (s *Store) SetStatus(ctx context.Context, reqID, to string, opts ...RequestUpdate) (Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback()

	var from string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM requests WHERE id = ?`, reqID).Scan(&from); err != nil {
		if err == sql.ErrNoRows {
			return Request{}, ErrNotFound
		}
		return Request{}, err
	}
	if from != to && !CanTransition(from, to) {
		return Request{}, fmt.Errorf("%w: illegal transition %s -> %s", ErrConflict, from, to)
	}

	u := RequestUpdate{}
	for _, o := range opts {
		o.mergeInto(&u)
	}
	sets := []string{"status = ?", "updated_at = ?"}
	args := []any{to, time.Now().UTC().Format(time.RFC3339)}
	if u.setError {
		sets = append(sets, "error = ?")
		args = append(args, u.errVal)
	}
	if u.setVersion {
		sets = append(sets, "version = ?")
		args = append(args, u.versionVal)
	}
	if u.setPlan {
		sets = append(sets, "plan_json = ?")
		args = append(args, u.planVal)
	}
	if u.setQuestion {
		sets = append(sets, "clarification_question = ?")
		args = append(args, u.questionVal)
	}
	if u.setAnswer {
		sets = append(sets, "clarification_answer = ?", "clarification_rounds = clarification_rounds + 1")
		args = append(args, u.answerVal)
	}
	args = append(args, reqID)
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET `+join(sets)+` WHERE id = ?`, args...); err != nil {
		return Request{}, err
	}
	if err := tx.Commit(); err != nil {
		return Request{}, err
	}
	return s.RequestByID(ctx, reqID)
}

// RequestUpdate carries optional field mutations for SetStatus.
type RequestUpdate struct {
	setError, setVersion, setPlan, setQuestion, setAnswer bool
	errVal, planVal, questionVal, answerVal               any
	versionVal                                            any
}

func (u RequestUpdate) mergeInto(dst *RequestUpdate) {
	if u.setError {
		dst.setError, dst.errVal = true, u.errVal
	}
	if u.setVersion {
		dst.setVersion, dst.versionVal = true, u.versionVal
	}
	if u.setPlan {
		dst.setPlan, dst.planVal = true, u.planVal
	}
	if u.setQuestion {
		dst.setQuestion, dst.questionVal = true, u.questionVal
	}
	if u.setAnswer {
		dst.setAnswer, dst.answerVal = true, u.answerVal
	}
}

func WithError(msg string) RequestUpdate  { return RequestUpdate{setError: true, errVal: msg} }
func WithVersion(n int) RequestUpdate     { return RequestUpdate{setVersion: true, versionVal: n} }
func WithPlan(json string) RequestUpdate  { return RequestUpdate{setPlan: true, planVal: json} }
func WithQuestion(q string) RequestUpdate { return RequestUpdate{setQuestion: true, questionVal: q} }
func WithAnswer(a string) RequestUpdate   { return RequestUpdate{setAnswer: true, answerVal: a} }
func ClearError() RequestUpdate           { return RequestUpdate{setError: true, errVal: nil} }

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}
