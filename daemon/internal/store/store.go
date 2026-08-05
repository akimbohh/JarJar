// Package store owns the SQLite database: schema migrations and all typed
// accessors. Both jarjard serve and jarjard worker open the same DB; WAL mode
// makes concurrent access safe. Per DATA-CONTRACTS.md §7 the daemon owns all
// tables and the worker writes only its own request row, versions, and events.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/id"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned by lookups that match no row.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned for unique-constraint / state conflicts.
var ErrConflict = errors.New("conflict")

type Store struct {
	db  *sql.DB
	hub *hub
}

// Open opens (creating if needed) the SQLite database at path, applies pending
// migrations, and returns a ready Store.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // single connection avoids WAL writer contention within a process
	s := &Store{db: db, hub: newHub()}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for advanced/manual use (tests).
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, name, nowRFC()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func parseRFC(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// HashToken returns hex(sha256(token)) as stored in players.token_hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- Players ----

// CreatePlayer inserts a player. The plaintext token is hashed before storage.
func (s *Store) CreatePlayer(ctx context.Context, name, token, role string) (Player, error) {
	p := Player{ID: newPlayerID(), Name: name, Role: role, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO players (id, name, token_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Name, HashToken(token), p.Role, p.CreatedAt.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return Player{}, ErrConflict
		}
		return Player{}, err
	}
	return p, nil
}

// PlayerByToken looks a player up by presented bearer token.
func (s *Store) PlayerByToken(ctx context.Context, token string) (Player, error) {
	return s.scanPlayer(s.db.QueryRowContext(ctx,
		`SELECT id, name, role, created_at FROM players WHERE token_hash = ?`, HashToken(token)))
}

func (s *Store) PlayerByID(ctx context.Context, id string) (Player, error) {
	return s.scanPlayer(s.db.QueryRowContext(ctx,
		`SELECT id, name, role, created_at FROM players WHERE id = ?`, id))
}

func (s *Store) ListPlayers(ctx context.Context) ([]Player, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, role, created_at FROM players ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Player
	for rows.Next() {
		p, err := s.scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePlayer removes a player, refusing to delete the last admin.
func (s *Store) DeletePlayer(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM players WHERE id = ?`, id).Scan(&role); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if role == RoleAdmin {
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE role = 'admin'`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return fmt.Errorf("%w: cannot delete the last admin", ErrConflict)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM players WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE role = 'admin'`).Scan(&n)
	return n, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanPlayer(row rowScanner) (Player, error) {
	var p Player
	var created string
	if err := row.Scan(&p.ID, &p.Name, &p.Role, &created); err != nil {
		if err == sql.ErrNoRows {
			return Player{}, ErrNotFound
		}
		return Player{}, err
	}
	p.CreatedAt = parseRFC(created)
	return p, nil
}

// ---- Invites ----

func (s *Store) CreateInvite(ctx context.Context, code, role string) (Invite, error) {
	inv := Invite{Code: code, Role: role, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (code, role, created_at) VALUES (?, ?, ?)`,
		inv.Code, inv.Role, inv.CreatedAt.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return Invite{}, ErrConflict
		}
		return Invite{}, err
	}
	return inv, nil
}

// RedeemInvite atomically consumes an unused invite and creates the player.
func (s *Store) RedeemInvite(ctx context.Context, code, name, token string) (Player, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Player{}, err
	}
	defer tx.Rollback()

	var role string
	var usedBy sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT role, used_by FROM invites WHERE code = ?`, code).Scan(&role, &usedBy)
	if err == sql.ErrNoRows {
		return Player{}, fmt.Errorf("%w: unknown invite code", ErrConflict)
	} else if err != nil {
		return Player{}, err
	}
	if usedBy.Valid {
		return Player{}, fmt.Errorf("%w: invite code already used", ErrConflict)
	}

	p := Player{ID: newPlayerID(), Name: name, Role: role, CreatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO players (id, name, token_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Name, HashToken(token), p.Role, p.CreatedAt.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return Player{}, fmt.Errorf("%w: player name already taken", ErrConflict)
		}
		return Player{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_by = ? WHERE code = ?`, p.ID, code); err != nil {
		return Player{}, err
	}
	if err := tx.Commit(); err != nil {
		return Player{}, err
	}
	return p, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func newPlayerID() string { return id.NewPlayer() }
