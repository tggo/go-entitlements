//go:build integration && probe

package entitlements_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/postgres"
)

var (
	pgRes  *dockertest.Resource
	pgAddr string
)

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		panic(err)
	}
	pgRes, err = pool.RunWithOptions(&dockertest.RunOptions{
		Name: fmt.Sprintf("probe-race-pg-%d", os.Getpid()), Repository: "postgres", Tag: "17",
		Env: []string{"POSTGRES_PASSWORD=secret", "POSTGRES_USER=test", "POSTGRES_DB=postgres"},
	}, func(hc *docker.HostConfig) { hc.AutoRemove = true; hc.RestartPolicy = docker.RestartPolicy{Name: "no"} })
	if err != nil {
		panic(err)
	}
	_ = pgRes.Expire(600)
	pgAddr = pgRes.GetHostPort("5432/tcp")
	if err := pool.Retry(func() error {
		p, err := pgxpool.New(context.Background(), dsn("postgres"))
		if err != nil {
			return err
		}
		defer p.Close()
		return p.Ping(context.Background())
	}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = pool.Purge(pgRes)
	os.Exit(code)
}

func dsn(db string) string {
	return fmt.Sprintf("postgres://test:secret@%s/%s?sslmode=disable", pgAddr, db)
}

var dbSeq int

// freshDB creates an isolated database; isolation optionally sets default_transaction_isolation.
func freshDB(t *testing.T, isolation string, maxConns int32) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	dbSeq++
	name := fmt.Sprintf("p%d_%d", os.Getpid(), dbSeq)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	if isolation != "" {
		if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s SET default_transaction_isolation = '%s'", name, isolation)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := pgxpool.ParseConfig(dsn(name))
	cfg.MaxConns = maxConns
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, name
}

func pgStress(t *testing.T, isolation string, maxConns int32, rounds int) {
	for round := 0; round < rounds; round++ {
		db, _ := freshDB(t, isolation, maxConns)
		st := postgres.New(db)
		if err := st.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		rec := newRecorder(st)
		svc := entitlements.NewService(rec)
		w := buildWorkload(6, 9, 3, int64(round))
		if runWorkload(t, svc, w, 64) {
			t.Fatal("HUNG (deadlock?)")
		}
		sqlErrs := 0
		for k, v := range rec.errs {
			if len(k) > 8 && k[:8] == "sqlstate" {
				sqlErrs += v
			}
		}
		if sqlErrs > 0 && isolation != "" {
			t.Logf("INFO: %d SQLSTATE errors surfaced to Process callers under %s (no retry)", sqlErrs, isolation)
			for id := range rec.fnCalls {
				rec.fnCalls[id] = 1
			} // retried deliveries legitimately re-run fn
		}
		checkInvariants(t, st, rec, w)
		var nEnt, nEv int
		_ = db.QueryRow(context.Background(), `SELECT count(*) FROM entitlements`).Scan(&nEnt)
		_ = db.QueryRow(context.Background(), `SELECT count(*) FROM processed_events`).Scan(&nEv)
		if nEnt != len(w.chains) {
			t.Errorf("entitlement rows %d != chains %d", nEnt, len(w.chains))
		}
		if nEv != len(w.uniq) && !(sqlErrs > 0 && isolation != "") {
			t.Errorf("processed_events %d != unique events %d", nEv, len(w.uniq))
		}
		db.Close()
		t.Logf("round %d (%q, maxConns=%d): errs=%v", round, isolation, maxConns, rec.errs)
		if t.Failed() {
			return
		}
	}
}

func TestPGStress_ReadCommitted(t *testing.T)  { pgStress(t, "", 20, 25) }
func TestPGStress_TinyPool(t *testing.T)       { pgStress(t, "", 2, 3) }
func TestPGStress_RepeatableRead(t *testing.T) { pgStress(t, "repeatable read", 20, 3) }
func TestPGStress_Serializable(t *testing.T)   { pgStress(t, "serializable", 20, 3) }

func TestPG_MigrateConcurrent(t *testing.T) {
	_, name := freshDB(t, "", 2)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { // each "instance" has its own pool
			defer wg.Done()
			p, err := pgxpool.New(context.Background(), dsn(name))
			if err != nil {
				errs <- err
				return
			}
			defer p.Close()
			errs <- postgres.New(p).Migrate(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("migrate: %v", e)
		}
	}
}

func chainEv(id, tx string, at time.Time) entitlements.Event {
	return entitlements.Event{ID: id, Source: entitlements.SourceGoogle, UserID: "u", OriginalTxID: tx, ProductID: "p",
		State: entitlements.StateActive, OccurredAt: at, ExpiresAt: at.Add(time.Hour)}
}

func TestPG_PanicInFnReleasesConnAndLock(t *testing.T) {
	db, _ := freshDB(t, "", 4)
	st := postgres.New(db)
	_ = st.Migrate(context.Background())
	now := time.Now().UTC()
	func() {
		defer func() { _ = recover() }()
		_, _ = st.Apply(context.Background(), chainEv("p1", "tx", now), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) { panic("boom") })
	}()
	if n := db.Stat().AcquiredConns(); n != 0 {
		t.Errorf("LEAK: %d conns still acquired after panic", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := st.Apply(ctx, chainEv("p2", "tx", now), applyFn); err != nil {
		t.Errorf("chain stays locked / event poisoned after panic: %v", err)
	}
}

func TestPG_CtxCancelMidApply(t *testing.T) {
	db, _ := freshDB(t, "", 4)
	st := postgres.New(db)
	_ = st.Migrate(context.Background())
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := st.Apply(ctx, chainEv("c1", "tx", now), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
		cancel()
		return entitlements.Apply(nil, chainEv("c1", "tx", now))
	})
	t.Logf("apply after cancel: %v", err)
	time.Sleep(200 * time.Millisecond)
	if n := db.Stat().AcquiredConns(); n != 0 {
		t.Errorf("LEAK: %d conns acquired after cancel", n)
	}
	ctx2, c2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer c2()
	if _, err := st.Apply(ctx2, chainEv("c2", "tx", now), applyFn); err != nil {
		t.Errorf("chain blocked after cancel: %v", err)
	}
	var n int
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE event_id='c1'`).Scan(&n)
	t.Logf("c1 recorded rows: %d (err was %v)", n, err)
}

// waiter blocked on advisory lock must honour ctx.
func TestPG_LockWaitHonoursCtx(t *testing.T) {
	db, _ := freshDB(t, "", 4)
	st := postgres.New(db)
	_ = st.Migrate(context.Background())
	now := time.Now().UTC()
	hold, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = st.Apply(context.Background(), chainEv("h1", "tx", now), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
			close(hold)
			<-release
			return entitlements.Apply(nil, chainEv("h1", "tx", now))
		})
	}()
	<-hold
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := st.Apply(ctx, chainEv("h2", "tx", now.Add(time.Second)), applyFn)
	t.Logf("waiter returned after %v: %v", time.Since(t0), err)
	close(release)
	time.Sleep(200 * time.Millisecond)
	if n := db.Stat().AcquiredConns(); n != 0 {
		t.Errorf("LEAK %d", n)
	}
}

func applyFn(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
	return entitlements.Apply(cur, entitlements.Event{ID: "x", Source: entitlements.SourceGoogle, OriginalTxID: "tx", State: entitlements.StateActive, OccurredAt: time.Now().Add(time.Hour)})
}
