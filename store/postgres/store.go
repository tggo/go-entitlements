// Package postgres is a pgx-backed entitlements.Store.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tggo/go-entitlements"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockID is the advisory lock key that serialises concurrent Migrate calls.
const migrateLockID = 7441980251

// Store persists entitlements in Postgres. It is safe for concurrent use.
// processed_events.raw keeps full webhook payloads (possibly personal data)
// until Prune removes them.
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
		return fmt.Errorf("postgres: begin migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("postgres: migrate lock: %w", err)
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS entitlements_schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("postgres: create migrations table: %w", err)
	}
	files, err := fsys.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("postgres: read migrations: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM entitlements_schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return fmt.Errorf("postgres: check migration %s: %w", name, err)
		}
		if done {
			continue
		}
		sql, rerr := fsys.ReadFile("migrations/" + name)
		if rerr != nil {
			return fmt.Errorf("postgres: read migration %s: %w", name, rerr)
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("postgres: migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO entitlements_schema_migrations (name) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("postgres: record migration %s: %w", name, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit migrate: %w", err)
	}
	return nil
}

func recordable(err error) bool {
	return errors.Is(err, entitlements.ErrStale) ||
		errors.Is(err, entitlements.ErrIgnored) ||
		errors.Is(err, entitlements.ErrUnsupported)
}

const selectCols = `source, original_tx_id, user_id, product_id, state, expires_at, updated_at, version, environment`

func scan(row pgx.Row) (*entitlements.Entitlement, error) {
	var (
		e       entitlements.Entitlement
		source  string
		state   string
		expires *time.Time
		env     string
	)
	if err := row.Scan(&source, &e.OriginalTxID, &e.UserID, &e.ProductID, &state, &expires, &e.UpdatedAt, &e.Version, &env); err != nil {
		return nil, err
	}
	e.Source = entitlements.Source(source)
	e.State = entitlements.State(state)
	e.Environment = entitlements.Environment(env)
	e.UpdatedAt = e.UpdatedAt.UTC()
	if expires != nil {
		e.ExpiresAt = expires.UTC()
	}
	return &e, nil
}

// finish records the outcome on the dedupe row and commits.
func finish(ctx context.Context, tx pgx.Tx, source entitlements.Source, eventID string, applied bool, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE processed_events SET applied = $3, reason = $4 WHERE source = $1 AND event_id = $2`, string(source), eventID, applied, reason); err != nil {
		return fmt.Errorf("postgres: finish event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit apply: %w", err)
	}
	return nil
}

// Apply implements entitlements.Store. The transaction runs at Read
// Committed whatever the database default is; the advisory and row locks give
// the per-chain serialisation. As a safeguard, serialization failures (40001)
// and deadlocks (40P01) are retried a bounded number of times with jittered
// backoff, so fn may be called more than once.
func (s *Store) Apply(ctx context.Context, ev entitlements.Event, fn func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	var out *entitlements.Entitlement
	err := withRetry(ctx, func() error {
		var err error
		out, err = s.applyOnce(ctx, ev, fn)
		return err
	})
	return out, err
}

const (
	maxAttempts   = 5
	retryBaseWait = 10 * time.Millisecond
)

// retryable reports serialization failures and deadlocks.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// backoff is retryBaseWait doubled per attempt, plus up to 100% jitter.
func backoff(attempt int) time.Duration {
	base := retryBaseWait << attempt
	return base + time.Duration(rand.Int64N(int64(base))) //nolint:gosec // jitter, not security
}

func withRetry(ctx context.Context, op func() error) error {
	var err error
	for attempt := range maxAttempts {
		if err = op(); err == nil || !retryable(err) {
			return err
		}
		wait := backoff(attempt)
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres: retry aborted: %w", errors.Join(ctx.Err(), err))
		case <-time.After(wait):
		}
	}
	return err
}

func (s *Store) applyOnce(ctx context.Context, ev entitlements.Event, fn func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("postgres: begin apply: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(ev.Source)+":"+ev.OriginalTxID); err != nil {
		return nil, fmt.Errorf("postgres: lock chain: %w", err)
	}

	tag, err := tx.Exec(ctx, `
INSERT INTO processed_events (event_id, source, original_tx_id, raw, applied, occurred_at)
VALUES ($1, $2, $3, $4, false, $5)
ON CONFLICT (source, event_id) DO NOTHING`,
		ev.ID, string(ev.Source), ev.OriginalTxID, ev.Raw, ev.OccurredAt)
	if err != nil {
		return nil, fmt.Errorf("postgres: record event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, entitlements.ErrDuplicate
	}

	cur, err := scan(tx.QueryRow(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE source = $1 AND original_tx_id = $2 FOR UPDATE`,
		string(ev.Source), ev.OriginalTxID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: load entitlement: %w", err)
	} // no row: cur is nil, a new chain

	next, fnErr := fn(cur)
	if fnErr != nil {
		if !recordable(fnErr) {
			return nil, fnErr
		}
		if err = finish(ctx, tx, ev.Source, ev.ID, false, fnErr.Error()); err != nil {
			return nil, err
		}
		return cur, fnErr
	}
	if next == nil {
		if err = finish(ctx, tx, ev.Source, ev.ID, false, "noop"); err != nil {
			return nil, err
		}
		return cur, nil
	}

	var expires *time.Time
	if !next.ExpiresAt.IsZero() {
		expires = &next.ExpiresAt
	}
	if _, err = tx.Exec(ctx, `
INSERT INTO entitlements (source, original_tx_id, user_id, product_id, state, expires_at, updated_at, version, environment)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (source, original_tx_id) DO UPDATE SET
    user_id = EXCLUDED.user_id, product_id = EXCLUDED.product_id, state = EXCLUDED.state,
    expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at, version = EXCLUDED.version,
    environment = EXCLUDED.environment`,
		string(ev.Source), ev.OriginalTxID, next.UserID, next.ProductID, string(next.State), expires, next.UpdatedAt, next.Version, string(next.Environment)); err != nil {
		return nil, fmt.Errorf("postgres: save entitlement: %w", err)
	}
	if err = finish(ctx, tx, ev.Source, ev.ID, true, ""); err != nil {
		return nil, err
	}
	out := *next
	out.Source = ev.Source
	out.OriginalTxID = ev.OriginalTxID
	return &out, nil
}

// Prune deletes processed-event rows (including their stored raw payloads)
// that occurred before the given time and returns how many were removed.
// Dedupe only protects against redeliveries within the retention window, so
// pick a window longer than the stores' retry horizon (days, not minutes).
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM processed_events WHERE occurred_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("postgres: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Get implements entitlements.Store.
func (s *Store) Get(ctx context.Context, source entitlements.Source, originalTxID string) (*entitlements.Entitlement, error) {
	e, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE source = $1 AND original_tx_id = $2`,
		string(source), originalTxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, entitlements.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get entitlement: %w", err)
	}
	return e, nil
}

// ListByUser implements entitlements.Store, ordered by (source, original_tx_id).
func (s *Store) ListByUser(ctx context.Context, userID string) ([]entitlements.Entitlement, error) {
	if userID == "" {
		return nil, nil // userless entitlements (no appAccountToken) are not one user's
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+selectCols+` FROM entitlements WHERE user_id = $1 ORDER BY source, original_tx_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list by user: %w", err)
	}
	defer rows.Close()
	var out []entitlements.Entitlement
	for rows.Next() {
		e, serr := scan(rows)
		if serr != nil {
			return nil, fmt.Errorf("postgres: scan entitlement: %w", serr)
		}
		out = append(out, *e)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list by user: %w", err)
	}
	return out, nil
}
