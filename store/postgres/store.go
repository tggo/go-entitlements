// Package postgres is a pgx-backed entitlements.Store.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tggo/go-entitlements"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockID is the advisory lock key that serialises concurrent Migrate calls.
const migrateLockID = 7441980251

// Store persists entitlements in Postgres.
//
// Apply runs in one transaction: advisory xact lock on the chain key (covers
// the row-creation race, where there is no row to lock yet), dedupe through
// INSERT ... ON CONFLICT DO NOTHING on processed_events, then SELECT ... FOR
// UPDATE on the chain row.
//
// Semantics match the memory store: a duplicate event ID returns ErrDuplicate
// without calling fn; ErrStale/ErrIgnored/ErrUnsupported from fn are recorded
// (applied=false, reason) and returned with the current entitlement; other fn
// errors roll back everything; fn returning (nil, nil) records the event and
// persists nothing.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store on pool. Call Migrate once before use.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ entitlements.Store = (*Store)(nil)

// Migrate applies the embedded SQL migrations, in file order, once each.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationsFS)
}

type migrationFS interface {
	fs.ReadDirFS
	fs.ReadFileFS
}

func (s *Store) migrate(ctx context.Context, fsys migrationFS) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fsys.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fsys.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func recordable(err error) bool {
	return errors.Is(err, entitlements.ErrStale) ||
		errors.Is(err, entitlements.ErrIgnored) ||
		errors.Is(err, entitlements.ErrUnsupported)
}

const selectCols = `source, original_tx_id, user_id, product_id, state, expires_at, updated_at, version`

func scan(row pgx.Row) (*entitlements.Entitlement, error) {
	var (
		e       entitlements.Entitlement
		source  string
		state   string
		expires *time.Time
	)
	if err := row.Scan(&source, &e.OriginalTxID, &e.UserID, &e.ProductID, &state, &expires, &e.UpdatedAt, &e.Version); err != nil {
		return nil, err
	}
	e.Source = entitlements.Source(source)
	e.State = entitlements.State(state)
	e.UpdatedAt = e.UpdatedAt.UTC()
	if expires != nil {
		e.ExpiresAt = expires.UTC()
	}
	return &e, nil
}

// finish records the outcome on the dedupe row and commits.
func finish(ctx context.Context, tx pgx.Tx, eventID string, applied bool, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE processed_events SET applied = $2, reason = $3 WHERE event_id = $1`, eventID, applied, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Apply implements entitlements.Store.
func (s *Store) Apply(ctx context.Context, ev entitlements.Event, fn func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(ev.Source)+":"+ev.OriginalTxID); err != nil {
		return nil, err
	}

	tag, err := tx.Exec(ctx, `
INSERT INTO processed_events (event_id, source, original_tx_id, raw, applied, occurred_at)
VALUES ($1, $2, $3, $4, false, $5)
ON CONFLICT (event_id) DO NOTHING`,
		ev.ID, string(ev.Source), ev.OriginalTxID, ev.Raw, ev.OccurredAt)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, entitlements.ErrDuplicate
	}

	cur, err := scan(tx.QueryRow(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE source = $1 AND original_tx_id = $2 FOR UPDATE`,
		string(ev.Source), ev.OriginalTxID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		cur = nil
	}

	next, fnErr := fn(cur)
	if fnErr != nil {
		if !recordable(fnErr) {
			return nil, fnErr
		}
		if err := finish(ctx, tx, ev.ID, false, fnErr.Error()); err != nil {
			return nil, err
		}
		return cur, fnErr
	}
	if next == nil {
		if err := finish(ctx, tx, ev.ID, false, "noop"); err != nil {
			return nil, err
		}
		return cur, nil
	}

	var expires *time.Time
	if !next.ExpiresAt.IsZero() {
		expires = &next.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO entitlements (source, original_tx_id, user_id, product_id, state, expires_at, updated_at, version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (source, original_tx_id) DO UPDATE SET
    user_id = EXCLUDED.user_id, product_id = EXCLUDED.product_id, state = EXCLUDED.state,
    expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at, version = EXCLUDED.version`,
		string(ev.Source), ev.OriginalTxID, next.UserID, next.ProductID, string(next.State), expires, next.UpdatedAt, next.Version); err != nil {
		return nil, err
	}
	if err := finish(ctx, tx, ev.ID, true, ""); err != nil {
		return nil, err
	}
	out := *next
	out.Source = ev.Source
	out.OriginalTxID = ev.OriginalTxID
	return &out, nil
}

// Get implements entitlements.Store.
func (s *Store) Get(ctx context.Context, source entitlements.Source, originalTxID string) (*entitlements.Entitlement, error) {
	e, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE source = $1 AND original_tx_id = $2`,
		string(source), originalTxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, entitlements.ErrNotFound
	}
	return e, err
}

// ListByUser implements entitlements.Store, ordered by (source, original_tx_id).
func (s *Store) ListByUser(ctx context.Context, userID string) ([]entitlements.Entitlement, error) {
	if userID == "" {
		return nil, nil // userless entitlements (no appAccountToken) are not one user's
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE user_id = $1 ORDER BY source, original_tx_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entitlements.Entitlement
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
