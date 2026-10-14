//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func (s *StoreSuite) TestCreateAndGet() {
	t0 := s.now
	got, err := s.process(s.ev("e1", entitlements.StateActive, t0))
	s.Require().NoError(err)
	s.Equal(int64(1), got.Version)

	e := s.get("tx1")
	s.Equal(entitlements.StateActive, e.State)
	s.Equal("u1", e.UserID)
	s.True(t0.Equal(e.UpdatedAt))
	s.True(t0.Add(30 * 24 * time.Hour).Equal(e.ExpiresAt))

	_, err = s.store.Get(context.Background(), entitlements.SourceApple, "missing")
	s.ErrorIs(err, entitlements.ErrNotFound)
}

func (s *StoreSuite) TestZeroExpiresAtRoundTrips() {
	e := s.ev("e1", entitlements.StateActive, s.now)
	e.ExpiresAt = time.Time{}
	_, err := s.process(e)
	s.Require().NoError(err)
	s.True(s.get("tx1").ExpiresAt.IsZero())
}

func (s *StoreSuite) TestDuplicateDelivery() {
	e := s.ev("e1", entitlements.StateActive, s.now)
	_, err := s.process(e)
	s.Require().NoError(err)
	_, err = s.process(e)
	s.ErrorIs(err, entitlements.ErrDuplicate)
	s.Equal(int64(1), s.get("tx1").Version)
	s.Equal(1, s.count("processed_events"))
}

func (s *StoreSuite) TestOutOfOrderIsStaleAndRecorded() {
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

func (s *StoreSuite) TestEqualTimestampApplied() {
	t0 := s.now
	_, err := s.process(s.ev("e1", entitlements.StateActive, t0))
	s.Require().NoError(err)
	_, err = s.process(s.ev("e2", entitlements.StateGrace, t0))
	s.Require().NoError(err)
	e := s.get("tx1")
	s.Equal(entitlements.StateGrace, e.State)
	s.Equal(int64(2), e.Version)
}

func (s *StoreSuite) TestRefundThenLateRenew() {
	t0 := s.now
	_, err := s.process(s.ev("e1", entitlements.StateActive, t0))
	s.Require().NoError(err)
	_, err = s.process(s.ev("e3", entitlements.StateRefunded, t0.Add(2*time.Hour)))
	s.Require().NoError(err)

	// renew that happened before the refund but arrived after it
	_, err = s.process(s.ev("e2", entitlements.StateActive, t0.Add(time.Hour)))
	s.ErrorIs(err, entitlements.ErrStale)
	// renewal-failure after the refund must not undo it either
	_, err = s.process(s.ev("e4", entitlements.StateBillingRetry, t0.Add(3*time.Hour)))
	s.ErrorIs(err, entitlements.ErrStale)

	e := s.get("tx1")
	s.Equal(entitlements.StateRefunded, e.State)
	s.Equal(int64(2), e.Version)
	ok, err := s.svc.IsActive(context.Background(), "u1", "pro")
	s.Require().NoError(err)
	s.False(ok)
}

func (s *StoreSuite) TestSupersedeChain() {
	t0 := s.now
	a := s.ev("e1", entitlements.StateActive, t0)
	a.Source, a.OriginalTxID = entitlements.SourceGoogle, "tokA"
	_, err := s.process(a)
	s.Require().NoError(err)

	b := s.ev("e2", entitlements.StateActive, t0.Add(time.Minute))
	b.Source, b.OriginalTxID, b.SupersedesTxID = entitlements.SourceGoogle, "tokB", "tokA"
	_, err = s.process(b)
	s.Require().NoError(err)
	_, err = s.process(b)
	s.ErrorIs(err, entitlements.ErrDuplicate)

	oldE, err := s.store.Get(context.Background(), entitlements.SourceGoogle, "tokA")
	s.Require().NoError(err)
	newE, err := s.store.Get(context.Background(), entitlements.SourceGoogle, "tokB")
	s.Require().NoError(err)
	s.Equal(entitlements.StateExpired, oldE.State)
	s.Equal(entitlements.StateActive, newE.State)

	ok, err := s.svc.IsActive(context.Background(), "u1", "pro")
	s.Require().NoError(err)
	s.True(ok)
}

func (s *StoreSuite) TestConcurrentDeliverySameChain() {
	const n = 40
	t0 := s.now
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// first writer creates the row: exercises the creation race too
			_, results[i] = s.process(s.ev(fmt.Sprintf("e%02d", i), entitlements.StateActive, t0.Add(time.Duration(i)*time.Second)))
		}()
	}
	wg.Wait()

	var applied, stale int
	for _, err := range results {
		switch {
		case err == nil:
			applied++
		case errors.Is(err, entitlements.ErrStale):
			stale++
		default:
			s.Failf("unexpected error", "%v", err)
		}
	}
	e := s.get("tx1")
	s.True(t0.Add((n-1)*time.Second).Equal(e.UpdatedAt), "final state must be the newest event")
	s.Equal(int64(applied), e.Version, "version counts applied events")
	s.Equal(1, s.count("entitlements"))
	s.Equal(n, s.count("processed_events"))
	s.Equal(n, applied+stale)
}

func (s *StoreSuite) TestConcurrentDuplicateSameID() {
	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	e := s.ev("same", entitlements.StateActive, s.now)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.process(e)
		}()
	}
	wg.Wait()
	var ok, dup int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, entitlements.ErrDuplicate):
			dup++
		default:
			s.Failf("unexpected error", "%v", err)
		}
	}
	s.Equal(1, ok)
	s.Equal(n-1, dup)
	s.Equal(int64(1), s.get("tx1").Version)
}

func (s *StoreSuite) TestListByUser() {
	t0 := s.now
	for i, tx := range []string{"txB", "txA"} {
		e := s.ev(fmt.Sprint("e", i), entitlements.StateActive, t0)
		e.OriginalTxID = tx
		_, err := s.process(e)
		s.Require().NoError(err)
	}
	other := s.ev("eo", entitlements.StateActive, t0)
	other.UserID, other.OriginalTxID = "u2", "txZ"
	_, err := s.process(other)
	s.Require().NoError(err)

	list, err := s.store.ListByUser(context.Background(), "u1")
	s.Require().NoError(err)
	s.Require().Len(list, 2)
	s.Equal("txA", list[0].OriginalTxID)
	s.Equal("txB", list[1].OriginalTxID)

	none, err := s.store.ListByUser(context.Background(), "nobody")
	s.Require().NoError(err)
	s.Empty(none)
}

func (s *StoreSuite) TestFnErrorRollsBack() {
	boom := fmt.Errorf("boom")
	_, err := s.store.Apply(context.Background(), s.ev("e1", entitlements.StateActive, s.now),
		func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { return nil, boom })
	s.ErrorIs(err, boom)
	s.Equal(0, s.count("processed_events"), "failed event must stay retryable")
	_, err = s.process(s.ev("e1", entitlements.StateActive, s.now))
	s.NoError(err)
}

func (s *StoreSuite) TestNilResultRecordsNoop() {
	cur, err := s.store.Apply(context.Background(), s.ev("e1", entitlements.StateActive, s.now),
		func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { return nil, nil })
	s.Require().NoError(err)
	s.Nil(cur)
	s.Equal(0, s.count("entitlements"))
	s.Equal(1, s.count("processed_events"))
}

func (s *StoreSuite) TestRawStored() {
	_, err := s.process(s.ev("e1", entitlements.StateActive, s.now))
	s.Require().NoError(err)
	var raw []byte
	s.Require().NoError(s.db.QueryRow(context.Background(), `SELECT raw FROM processed_events WHERE event_id='e1'`).Scan(&raw))
	s.JSONEq(`{"id":"e1"}`, string(raw))
}

func (s *StoreSuite) TestCancelledContext() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.store.Apply(ctx, s.ev("e1", entitlements.StateActive, s.now),
		func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { return nil, nil })
	s.Error(err)
	_, err = s.store.ListByUser(ctx, "u1")
	s.Error(err)
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

func (s *StoreSuite) TestListByUser_EmptyUserMatchesNothing() {
	e := s.ev("e-nouser", entitlements.StateActive, s.now)
	e.UserID = ""
	_, err := s.store.Apply(context.Background(), e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return entitlements.Apply(cur, e)
	})
	s.Require().NoError(err)
	list, err := s.store.ListByUser(context.Background(), "")
	s.Require().NoError(err)
	s.Empty(list)
}

func (s *StoreSuite) TestSandboxRoundTrip() {
	e := s.ev("e-sandbox", entitlements.StateActive, s.now)
	e.Sandbox = true
	got, err := s.process(e)
	s.Require().NoError(err)
	s.True(got.Sandbox)
	s.True(s.get("tx1").Sandbox)
	e2 := s.ev("e-prod", entitlements.StateActive, s.now.Add(time.Minute))
	_, err = s.process(e2)
	s.Require().NoError(err)
	s.False(s.get("tx1").Sandbox)
}

func (s *StoreSuite) TestSupersedeUnseenOldChainLeavesTombstone() {
	nw := s.ev("e-new", entitlements.StateActive, s.now)
	nw.OriginalTxID, nw.SupersedesTxID = "txNew", "txGhost"
	_, err := s.process(nw)
	s.Require().NoError(err)

	ghost := s.get("txGhost")
	s.Equal(entitlements.StateExpired, ghost.State)

	late := s.ev("e-late", entitlements.StateActive, s.now.Add(-time.Hour))
	late.OriginalTxID = "txGhost"
	_, err = s.process(late)
	s.ErrorIs(err, entitlements.ErrStale)
	s.Equal(entitlements.StateExpired, s.get("txGhost").State)
}

func (s *StoreSuite) TestDedupeIsPerSource() {
	a := s.ev("same-id", entitlements.StateActive, s.now)
	g := a
	g.Source, g.OriginalTxID = entitlements.SourceGoogle, "tokG"
	_, err := s.process(a)
	s.Require().NoError(err)
	_, err = s.process(g)
	s.Require().NoError(err, "same ID from another source is not a duplicate")
	_, err = s.process(a)
	s.ErrorIs(err, entitlements.ErrDuplicate)
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
	old := s.ev("e-old", entitlements.StateActive, s.now.Add(-48*time.Hour))
	old.OriginalTxID = "txOld"
	_, err := s.process(old)
	s.Require().NoError(err)
	_, err = s.process(s.ev("e-new", entitlements.StateActive, s.now))
	s.Require().NoError(err)

	n, err := s.store.Prune(context.Background(), s.now.Add(-24*time.Hour))
	s.Require().NoError(err)
	s.Equal(int64(1), n)
	s.Equal(1, s.count("processed_events"))
	s.NotNil(s.get("txOld"), "entitlements are kept")
}
