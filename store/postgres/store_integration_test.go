//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/suite"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/postgres"
	"github.com/tggo/go-entitlements/storetest"
)

type StoreSuite struct {
	suite.Suite
	pool     *dockertest.Pool
	resource *dockertest.Resource
	db       *pgxpool.Pool
	store    *postgres.Store
	svc      *entitlements.Service
	dsn      string
	now      time.Time
}

func TestStoreSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	suite.Run(t, new(StoreSuite))
}

func (s *StoreSuite) SetupSuite() {
	var err error
	s.pool, err = dockertest.NewPool("")
	s.Require().NoError(err)
	s.Require().NoError(s.pool.Client.Ping())

	s.resource, err = s.pool.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("go-entitlements-pg-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Repository: "postgres",
		Tag:        "17",
		Env:        []string{"POSTGRES_PASSWORD=secret", "POSTGRES_USER=test", "POSTGRES_DB=entitlements"},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	s.Require().NoError(err)
	s.Require().NoError(s.resource.Expire(300))

	dsn := fmt.Sprintf("postgres://test:secret@%s/entitlements?sslmode=disable", s.resource.GetHostPort("5432/tcp"))
	s.dsn = dsn
	s.Require().NoError(s.pool.Retry(func() error {
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return err
		}
		if err := p.Ping(context.Background()); err != nil {
			p.Close()
			return err
		}
		s.db = p
		return nil
	}))

	s.store = postgres.New(s.db)
	s.Require().NoError(s.store.Migrate(context.Background()))
	s.Require().NoError(s.store.Migrate(context.Background()), "migrate must be idempotent")
}

func (s *StoreSuite) TearDownSuite() {
	if s.db != nil {
		s.db.Close()
	}
	if s.pool != nil && s.resource != nil {
		_ = s.pool.Purge(s.resource)
	}
}

func (s *StoreSuite) SetupTest() {
	s.now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.svc = entitlements.NewService(s.store,
		entitlements.WithLogger(testutil.NewLogger(s.T())),
		entitlements.WithTimeFn(func() time.Time { return s.now }))
}

func (s *StoreSuite) TearDownTest() {
	_, err := s.db.Exec(context.Background(), `TRUNCATE entitlements, processed_events`)
	s.Require().NoError(err)
}

func (s *StoreSuite) ev(id string, state entitlements.State, at time.Time) entitlements.Event {
	return entitlements.Event{ID: id, Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "tx1",
		ProductID: "pro", State: state, ExpiresAt: at.Add(30 * 24 * time.Hour), OccurredAt: at, Raw: []byte(`{"id":"` + id + `"}`)}
}

func (s *StoreSuite) process(e entitlements.Event) (*entitlements.Entitlement, error) {
	return s.svc.Process(context.Background(), e)
}

func (s *StoreSuite) get(tx string) *entitlements.Entitlement {
	e, err := s.store.Get(context.Background(), entitlements.SourceApple, tx)
	s.Require().NoError(err)
	return e
}

func (s *StoreSuite) count(table string) int {
	var n int
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

func (s *StoreSuite) TestStaleEventIsRecordedWithReason() {
	t0 := s.now
	_, err := s.process(s.ev("e2", entitlements.StateActive, t0.Add(time.Hour)))
	s.Require().NoError(err)

	cur, err := s.process(s.ev("e1", entitlements.StateExpired, t0))
	s.ErrorIs(err, entitlements.ErrStale)
	s.Require().NotNil(cur)
	s.Equal(entitlements.StateActive, s.get("tx1").State)
	s.Equal(int64(1), s.get("tx1").Version)

	var applied bool
	var reason string
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT applied, reason FROM processed_events WHERE event_id = 'e1'`).Scan(&applied, &reason))
	s.False(applied)
	s.Contains(reason, "stale")
	_, err = s.process(s.ev("e1", entitlements.StateExpired, t0))
	s.ErrorIs(err, entitlements.ErrDuplicate)
}

func (s *StoreSuite) TestRawStored() {
	_, err := s.process(s.ev("e1", entitlements.StateActive, s.now))
	s.Require().NoError(err)
	var raw []byte
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT raw FROM processed_events WHERE event_id='e1'`).Scan(&raw))
	s.JSONEq(`{"id":"e1"}`, string(raw))
}

func (s *StoreSuite) TestClosedPoolErrors() {
	ctx := context.Background()
	p, err := pgxpool.New(ctx, s.db.Config().ConnString())
	s.Require().NoError(err)
	p.Close()
	st := postgres.New(p)

	s.Error(st.Migrate(ctx))
	_, err = st.Apply(ctx, s.ev("e1", entitlements.StateActive, s.now),
		func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { return nil, nil })
	s.Error(err)
	_, err = st.Get(ctx, entitlements.SourceApple, "tx1")
	s.Error(err)
	s.NotErrorIs(err, entitlements.ErrNotFound)
	_, err = st.ListByUser(ctx, "u1")
	s.Error(err)
}

func (s *StoreSuite) TestApplyFailsWhenTablesMissing() {
	ctx := context.Background()
	_, err := s.db.Exec(ctx, `ALTER TABLE entitlements RENAME TO entitlements_x`)
	s.Require().NoError(err)
	defer func() { _, _ = s.db.Exec(ctx, `ALTER TABLE entitlements_x RENAME TO entitlements`) }()

	_, err = s.process(s.ev("e1", entitlements.StateActive, s.now))
	s.Error(err)
	_, err = s.store.ListByUser(ctx, "u1")
	s.Error(err)
	s.Equal(0, s.count("processed_events"), "failed apply must roll back the dedupe row")
}

func (s *StoreSuite) TestMigrateFailures() {
	ctx := context.Background()
	// a broken migration must fail and leave no entitlements_schema_migrations row behind
	err := s.store.MigrateFS(ctx, fstest.MapFS{"migrations/999_bad.sql": {Data: []byte("THIS IS NOT SQL")}})
	s.Error(err)
	var n int
	s.Require().NoError(s.db.QueryRow(ctx, `SELECT count(*) FROM entitlements_schema_migrations WHERE name = '999_bad.sql'`).Scan(&n))
	s.Equal(0, n)

	// missing migrations dir
	s.Error(s.store.MigrateFS(ctx, fstest.MapFS{}))
}

// With the database defaulting to SERIALIZABLE, Apply must still never
// surface 40001: it pins READ COMMITTED itself.
func (s *StoreSuite) TestApplyUnderSerializableDefault() {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(s.dsn)
	s.Require().NoError(err)
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	s.Require().NoError(err)
	defer pool.Close()
	var iso string
	s.Require().NoError(pool.QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&iso))
	s.Require().Equal("serializable", iso)

	st := postgres.New(pool)
	svc := entitlements.NewService(st)
	const chains, perChain = 4, 8
	var wg sync.WaitGroup
	errs := make(chan error, chains*perChain)
	for c := range chains {
		for i := range perChain {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e := s.ev(fmt.Sprintf("ser-%d-%d", c, i), entitlements.StateActive, s.now.Add(time.Duration(i)*time.Second))
				e.OriginalTxID = fmt.Sprintf("serTx%d", c)
				_, err := svc.Process(ctx, e)
				errs <- err
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, entitlements.ErrStale) {
			s.Require().NoError(err)
		}
	}
	for c := range chains {
		s.NotNil(s.get(fmt.Sprintf("serTx%d", c)))
	}
}

func (s *StoreSuite) TestPrune() {
	ctx := context.Background()
	cutoff := s.now.Add(-24 * time.Hour)
	for i, at := range []time.Time{cutoff.Add(-time.Hour), cutoff, cutoff.Add(time.Hour)} {
		e := s.ev(fmt.Sprintf("e%d", i), entitlements.StateActive, at)
		e.OriginalTxID = fmt.Sprintf("tx%d", i)
		_, err := s.process(e)
		s.Require().NoError(err)
	}

	n, err := s.store.Prune(ctx, cutoff)
	s.Require().NoError(err)
	s.Equal(int64(1), n)
	rows, err := s.db.Query(ctx, `SELECT event_id FROM processed_events ORDER BY event_id`)
	s.Require().NoError(err)
	defer rows.Close()
	var left []string
	for rows.Next() {
		var id string
		s.Require().NoError(rows.Scan(&id))
		left = append(left, id)
	}
	s.Equal([]string{"e1", "e2"}, left, "the older row goes; the row exactly at the cutoff and the newer one stay")
	for i := range 3 {
		s.NotNil(s.get(fmt.Sprintf("tx%d", i)), "entitlements are kept")
	}
}

func (s *StoreSuite) TestProcessedEventOutcomes() {
	ctx := context.Background()
	_, err := s.process(s.ev("ok", entitlements.StateActive, s.now.Add(time.Hour)))
	s.Require().NoError(err)
	_, err = s.process(s.ev("late", entitlements.StateExpired, s.now))
	s.Require().ErrorIs(err, entitlements.ErrStale)
	_, err = s.store.Apply(ctx, s.ev("noop", entitlements.StateActive, s.now), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { return nil, nil })
	s.Require().NoError(err)

	type row struct {
		applied bool
		reason  string
	}
	got := map[string]row{}
	rows, err := s.db.Query(ctx, `SELECT event_id, applied, reason FROM processed_events`)
	s.Require().NoError(err)
	defer rows.Close()
	for rows.Next() {
		var id string
		var r row
		s.Require().NoError(rows.Scan(&id, &r.applied, &r.reason))
		got[id] = r
	}
	s.Equal(true, got["ok"].applied)
	s.Empty(got["ok"].reason)
	s.False(got["late"].applied)
	s.Contains(got["late"].reason, "stale")
	s.False(got["noop"].applied)
	s.Equal("noop", got["noop"].reason)
}

func (s *StoreSuite) TestTimestampsComeBackInUTC() {
	cfg, perr := pgxpool.ParseConfig(s.dsn)
	s.Require().NoError(perr)
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Tokyo"
	pool, perr := pgxpool.NewWithConfig(context.Background(), cfg)
	s.Require().NoError(perr)
	defer pool.Close()
	st := postgres.New(pool)
	e := s.ev("tz", entitlements.StateActive, s.now)
	_, err := entitlements.NewService(st).Process(context.Background(), e)
	s.Require().NoError(err)
	got, err := st.Get(context.Background(), entitlements.SourceApple, "tx1")
	s.Require().NoError(err)
	s.Equal(time.UTC, got.UpdatedAt.Location())
	s.Equal(time.UTC, got.ExpiresAt.Location())
	s.True(got.UpdatedAt.Equal(s.now))
}

// The shared Store contract; each subtest starts from empty tables.
func (s *StoreSuite) TestConformance() {
	storetest.Run(s.T(), func(t *testing.T) entitlements.Store {
		t.Helper()
		_, err := s.db.Exec(context.Background(), `TRUNCATE entitlements, processed_events`)
		s.Require().NoError(err)
		return s.store
	})
}

func (s *StoreSuite) processedCount(db *pgxpool.Pool) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		var n int
		err := db.QueryRow(ctx, `SELECT count(*) FROM processed_events`).Scan(&n)
		return n, err
	}
}

func (s *StoreSuite) TestStress() {
	storetest.RunStress(s.T(), func(t *testing.T) entitlements.Store {
		t.Helper()
		_, err := s.db.Exec(context.Background(), `TRUNCATE entitlements, processed_events`)
		s.Require().NoError(err)
		return s.store
	}, storetest.StressConfig{
		Goroutines: 32, Chains: 6, EventsPerChain: 9, DupRate: 3, CrossSupersede: true, Rounds: 10,
		ProcessedCount: s.processedCount(s.db),
	})
}

// Two connections for 64 goroutines: the advisory lock must queue, not deadlock.
func (s *StoreSuite) TestStressTinyPool() {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(s.dsn)
	s.Require().NoError(err)
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	s.Require().NoError(err)
	defer pool.Close()
	storetest.RunStress(s.T(), func(t *testing.T) entitlements.Store {
		t.Helper()
		_, err := pool.Exec(ctx, `TRUNCATE entitlements, processed_events`)
		s.Require().NoError(err)
		return postgres.New(pool)
	}, storetest.StressConfig{
		Goroutines: 64, Chains: 6, EventsPerChain: 9, DupRate: 2, CrossSupersede: true, Rounds: 3,
		ProcessedCount: s.processedCount(pool),
	})
}

func (s *StoreSuite) TestStressUnderSerializableDefault() {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(s.dsn)
	s.Require().NoError(err)
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	s.Require().NoError(err)
	defer pool.Close()
	storetest.RunStress(s.T(), func(t *testing.T) entitlements.Store {
		t.Helper()
		_, err := pool.Exec(ctx, `TRUNCATE entitlements, processed_events`)
		s.Require().NoError(err)
		return postgres.New(pool)
	}, storetest.StressConfig{
		Goroutines: 64, Chains: 6, EventsPerChain: 9, DupRate: 3, CrossSupersede: true, Rounds: 3,
		ProcessedCount: s.processedCount(pool),
	})
}

func (s *StoreSuite) TestMigrateConcurrentInstances() {
	ctx := context.Background()
	_, err := s.db.Exec(ctx, `CREATE DATABASE migrate_race`)
	s.Require().NoError(err)
	dsn := strings.Replace(s.dsn, "/entitlements?", "/migrate_race?", 1)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { // each "instance" has its own pool
			defer wg.Done()
			p, err := pgxpool.New(ctx, dsn)
			if err != nil {
				errs <- err
				return
			}
			defer p.Close()
			errs <- postgres.New(p).Migrate(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		s.NoError(err)
	}
}

func (s *StoreSuite) TestPanicInFnReleasesConnectionAndLock() {
	ctx := context.Background()
	e := s.ev("panic-1", entitlements.StateActive, s.now)
	func() {
		defer func() { _ = recover() }()
		_, _ = s.store.Apply(ctx, e, func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { panic("boom") })
	}()
	s.Eventually(func() bool { return s.db.Stat().AcquiredConns() == 0 }, 2*time.Second, 20*time.Millisecond, "connection leaked after panic")
	_, err := s.process(s.ev("panic-2", entitlements.StateActive, s.now.Add(time.Second)))
	s.Require().NoError(err)
	s.Equal(int64(1), s.get("tx1").Version, "chain lock was released and the panicking event left no trace")
}

// A caller waiting on a chain lock must give up when its context ends.
func (s *StoreSuite) TestLockWaitHonoursContext() {
	hold, release := make(chan struct{}), make(chan struct{})
	e1 := s.ev("hold-1", entitlements.StateActive, s.now)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.store.Apply(context.Background(), e1, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
			close(hold)
			<-release
			return entitlements.Apply(cur, e1)
		})
	}()
	<-hold
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	e2 := s.ev("hold-2", entitlements.StateActive, s.now.Add(time.Second))
	_, err := s.store.Apply(ctx, e2, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return entitlements.Apply(cur, e2)
	})
	s.Error(err)
	s.Less(time.Since(begin), 3*time.Second)
	close(release)
	<-done
	s.Eventually(func() bool { return s.db.Stat().AcquiredConns() == 0 }, 2*time.Second, 20*time.Millisecond)
	_, err = s.process(s.ev("hold-3", entitlements.StateActive, s.now.Add(2*time.Second)))
	s.NoError(err, "the timed-out event stays retryable")
}

// Migration 004 turns the old sandbox flag into an environment.
func (s *StoreSuite) TestMigration004ConvertsSandboxFlag() {
	ctx := context.Background()
	_, err := s.db.Exec(ctx, `CREATE DATABASE migrate_env`)
	s.Require().NoError(err)
	dsn := strings.Replace(s.dsn, "/entitlements?", "/migrate_env?", 1)
	pool, err := pgxpool.New(ctx, dsn)
	s.Require().NoError(err)
	defer pool.Close()
	st := postgres.New(pool)

	old := fstest.MapFS{}
	for _, name := range []string{"001_init.sql", "002_sandbox.sql", "003_dedupe_per_source.sql"} {
		b, err := os.ReadFile("migrations/" + name)
		s.Require().NoError(err)
		old["migrations/"+name] = &fstest.MapFile{Data: b}
	}
	s.Require().NoError(st.MigrateFS(ctx, old))
	_, err = pool.Exec(ctx, `INSERT INTO entitlements (source, original_tx_id, user_id, product_id, state, updated_at, version, sandbox)
		VALUES ('apple','a','u','p','active',now(),1,true), ('apple','b','u','p','active',now(),1,false)`)
	s.Require().NoError(err)

	s.Require().NoError(st.Migrate(ctx))
	a, err := st.Get(ctx, entitlements.SourceApple, "a")
	s.Require().NoError(err)
	b, err := st.Get(ctx, entitlements.SourceApple, "b")
	s.Require().NoError(err)
	s.Equal(entitlements.EnvironmentSandbox, a.Environment)
	s.Equal(entitlements.Environment(""), b.Environment)
}

func (s *StoreSuite) TestDeleteUserClearsRawPayloadsButKeepsEventIDs() {
	ctx := context.Background()
	mine := s.ev("mine", entitlements.StateActive, s.now)
	theirs := s.ev("theirs", entitlements.StateActive, s.now)
	theirs.OriginalTxID, theirs.UserID = "tx-other", "u2"
	for _, e := range []entitlements.Event{mine, theirs} {
		_, err := s.process(e)
		s.Require().NoError(err)
	}
	// a stale event on the user's chain also carries a payload
	_, err := s.process(s.ev("mine-old", entitlements.StateExpired, s.now.Add(-time.Hour)))
	s.Require().ErrorIs(err, entitlements.ErrStale)

	n, err := s.store.DeleteUser(ctx, "u1")
	s.Require().NoError(err)
	s.Equal(1, n)

	rows, err := s.db.Query(ctx, `SELECT event_id, raw IS NULL FROM processed_events ORDER BY event_id`)
	s.Require().NoError(err)
	defer rows.Close()
	rawGone := map[string]bool{}
	for rows.Next() {
		var id string
		var gone bool
		s.Require().NoError(rows.Scan(&id, &gone))
		rawGone[id] = gone
	}
	s.Equal(map[string]bool{"mine": true, "mine-old": true, "theirs": false}, rawGone)
}

func (s *StoreSuite) TestAutoRenewIsNullUntilReported() {
	_, err := s.process(s.ev("e1", entitlements.StateActive, s.now))
	s.Require().NoError(err)
	var isNull bool
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT auto_renew IS NULL FROM entitlements`).Scan(&isNull))
	s.True(isNull)
}
