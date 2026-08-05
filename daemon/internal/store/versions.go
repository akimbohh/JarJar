package store

import (
	"context"
	"database/sql"
	"time"
)

// CreateVersion inserts a published version row. This is the atomic
// "version exists" point in the pipeline (PIPELINE.md §7).
func (s *Store) CreateVersion(ctx context.Context, v Version) error {
	var reqID any
	if v.RequestID != nil {
		reqID = *v.RequestID
	}
	if v.Status == "" {
		v.Status = "published"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO versions (number, created_at, request_id, git_commit, summary, status, manifest_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.Number, v.CreatedAt.UTC().Format(time.RFC3339), reqID, v.GitCommit, v.Summary, v.Status, v.ManifestPath)
	return err
}

func (s *Store) scanVersion(row rowScanner) (Version, error) {
	var v Version
	var created string
	var reqID sql.NullString
	if err := row.Scan(&v.Number, &created, &reqID, &v.GitCommit, &v.Summary, &v.Status, &v.ManifestPath); err != nil {
		if err == sql.ErrNoRows {
			return Version{}, ErrNotFound
		}
		return Version{}, err
	}
	v.CreatedAt = parseRFC(created)
	v.RequestID = nullStr(reqID)
	return v, nil
}

func (s *Store) VersionByNumber(ctx context.Context, n int) (Version, error) {
	return s.scanVersion(s.db.QueryRowContext(ctx,
		`SELECT number, created_at, request_id, git_commit, summary, status, manifest_path FROM versions WHERE number = ?`, n))
}

// CurrentVersion returns the highest version number, or (Version{}, false) if
// no version has been published yet.
func (s *Store) CurrentVersion(ctx context.Context) (Version, bool, error) {
	v, err := s.scanVersion(s.db.QueryRowContext(ctx,
		`SELECT number, created_at, request_id, git_commit, summary, status, manifest_path
		 FROM versions ORDER BY number DESC LIMIT 1`))
	if err == ErrNotFound {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	return v, true, nil
}

// ListVersions returns versions newest-first. before=0 means from the top.
func (s *Store) ListVersions(ctx context.Context, limit, before int) ([]Version, error) {
	q := `SELECT number, created_at, request_id, git_commit, summary, status, manifest_path FROM versions`
	args := []any{}
	if before > 0 {
		q += ` WHERE number < ?`
		args = append(args, before)
	}
	q += ` ORDER BY number DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := s.scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
