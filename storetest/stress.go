package storetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tggo/go-entitlements"
)

// StressConfig sizes RunStress.
type StressConfig struct {
	Goroutines     int // concurrent deliverers
	Chains         int // independent subscription chains (Google source)
	EventsPerChain int // distinct events per chain
	DupRate        int // extra deliveries of every event (0 = each delivered once)
	CrossSupersede bool
	Rounds         int           // repetitions, each on a fresh store and a fresh shuffle
	Timeout        time.Duration // per round; a round that exceeds it fails as a suspected deadlock

	// ProcessedCount, if set, returns how many processed-event rows the
	// backend holds; it must equal the number of distinct (source, id) pairs,
	// including the synthetic supersede events. Stores without a cheap count
	// leave it nil: the suite then proves every ID was recorded by
	// redelivering each one and expecting ErrDuplicate.
	ProcessedCount func(ctx context.Context) (int, error)
}

// Preset intensities, selectable with STORETEST_STRESS=light|heavy.
var (
	StressLight = StressConfig{Goroutines: 16, Chains: 4, EventsPerChain: 6, DupRate: 2, CrossSupersede: true, Rounds: 3, Timeout: time.Minute}
	StressHeavy = StressConfig{Goroutines: 64, Chains: 8, EventsPerChain: 12, DupRate: 3, CrossSupersede: true, Rounds: 25, Timeout: 2 * time.Minute}
)

// RunStress delivers shuffled, duplicated events across several chains from
// many goroutines through entitlements.Service, including supersedes that
// cross (A retires B while B retires A), and checks the invariants:
//
//   - the only errors are ErrDuplicate and ErrStale;
//   - one row per chain, and Version equals the number of applied events;
//   - the final state is that of the newest applied event, and no own event
//     is lost;
//   - every distinct (source, event ID) is recorded exactly once;
//   - nothing hangs within Timeout.
//
// It is skipped under -short. Set STORETEST_STRESS=light or heavy to override
// the intensity given in cfg. Each round gets its own store from newStore.
func RunStress(t *testing.T, newStore func(t *testing.T) entitlements.Store, cfg StressConfig) {
	t.Helper()
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}
	switch os.Getenv("STORETEST_STRESS") {
	case "light":
		cfg.Goroutines, cfg.Chains, cfg.EventsPerChain, cfg.DupRate, cfg.CrossSupersede, cfg.Rounds = StressLight.Goroutines, StressLight.Chains, StressLight.EventsPerChain, StressLight.DupRate, StressLight.CrossSupersede, StressLight.Rounds
	case "heavy":
		cfg.Goroutines, cfg.Chains, cfg.EventsPerChain, cfg.DupRate, cfg.CrossSupersede, cfg.Rounds = StressHeavy.Goroutines, StressHeavy.Chains, StressHeavy.EventsPerChain, StressHeavy.DupRate, StressHeavy.CrossSupersede, StressHeavy.Rounds
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Minute
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 1
	}
	if cfg.CrossSupersede && cfg.Chains < 2 {
		cfg.Chains = 2
	}

	var total int
	start := time.Now()
	for round := range cfg.Rounds {
		seed := uint64(round) + 1
		w := buildWorkload(cfg, seed)
		st := newStore(t)
		rec := &recorder{Store: st, applied: map[string][]entitlements.Event{}}
		svc := entitlements.NewService(rec)
		if hung := deliver(t, svc, w, cfg); hung {
			t.Fatalf("round %d (seed %d): no progress within %v, suspected deadlock", round, seed, cfg.Timeout)
		}
		total += len(w.events)
		checkInvariants(t, st, rec, w, cfg)
		if t.Failed() {
			t.Fatalf("round %d failed (seed %d, goroutines %d, chains %d)", round, seed, cfg.Goroutines, cfg.Chains)
		}
	}
	el := time.Since(start)
	t.Logf("stress: %d rounds, %d deliveries in %v (%.0f events/s), %d goroutines, %d chains",
		cfg.Rounds, total, el.Round(time.Millisecond), float64(total)/el.Seconds(), cfg.Goroutines, cfg.Chains)
}

// recorder wraps a Store and remembers which events produced a new state.
type recorder struct {
	entitlements.Store
	mu      sync.Mutex
	applied map[string][]entitlements.Event // chain key -> events that changed the entitlement
}

func chainKey(source entitlements.Source, tx string) string { return string(source) + ":" + tx }

func (r *recorder) Apply(ctx context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	var next *entitlements.Entitlement
	var fnErr error
	out, err := r.Store.Apply(ctx, ev, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		next, fnErr = fn(cur) // the last call wins if the store retries
		return next, fnErr
	})
	if err == nil && next != nil && fnErr == nil {
		r.mu.Lock()
		k := chainKey(ev.Source, ev.OriginalTxID)
		r.applied[k] = append(r.applied[k], ev)
		r.mu.Unlock()
	}
	return out, err
}

type workload struct {
	events []entitlements.Event // shuffled, with duplicates
	chains []string
	ownMax map[string]time.Time
	unique map[string]bool // event IDs including synthetic supersede events
}

func buildWorkload(cfg StressConfig, seed uint64) workload {
	rnd := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := workload{ownMax: map[string]time.Time{}, unique: map[string]bool{}}
	for i := range cfg.Chains {
		w.chains = append(w.chains, fmt.Sprintf("tok%d", i))
	}
	states := []entitlements.State{entitlements.StateActive, entitlements.StateGrace, entitlements.StateBillingRetry, entitlements.StateActive}
	var evs []entitlements.Event
	for i, c := range w.chains {
		for j := 1; j <= cfg.EventsPerChain; j++ {
			at := base.Add(time.Duration(j)*time.Minute + time.Duration(i)*time.Second)
			e := entitlements.Event{
				ID: fmt.Sprintf("%s-e%d", c, j), Source: entitlements.SourceGoogle, UserID: "u", OriginalTxID: c,
				ProductID: "pro", State: states[j%len(states)], OccurredAt: at, ExpiresAt: at.Add(time.Hour),
			}
			if cfg.CrossSupersede && j%3 == 0 {
				// chain i retires i+1; with i+1 doing the same to i+2 (and, for
				// two chains, back to i) the retirements cross.
				e.SupersedesTxID = w.chains[(i+1)%cfg.Chains]
				w.unique[e.ID+"#supersede"] = true
			}
			evs = append(evs, e)
			w.unique[e.ID] = true
			if at.After(w.ownMax[c]) {
				w.ownMax[c] = at
			}
		}
	}
	n := len(evs)
	for range cfg.DupRate {
		evs = append(evs, evs[:n]...)
	}
	rnd.Shuffle(len(evs), func(a, b int) { evs[a], evs[b] = evs[b], evs[a] })
	w.events = evs
	return w
}

func deliver(t *testing.T, svc *entitlements.Service, w workload, cfg StressConfig) (hung bool) {
	t.Helper()
	ch := make(chan entitlements.Event)
	var wg sync.WaitGroup
	var mu sync.Mutex
	bad := map[string]bool{}
	for range cfg.Goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range ch {
				ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
				_, err := svc.Process(ctx, ev)
				cancel()
				if err != nil && !errors.Is(err, entitlements.ErrDuplicate) && !errors.Is(err, entitlements.ErrStale) {
					mu.Lock()
					bad[err.Error()] = true
					mu.Unlock()
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		for _, e := range w.events {
			ch <- e
		}
		close(ch)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cfg.Timeout):
		return true
	}
	for msg := range bad {
		t.Errorf("unexpected Process error: %s", msg)
	}
	return false
}

func checkInvariants(t *testing.T, st entitlements.Store, rec *recorder, w workload, cfg StressConfig) {
	t.Helper()
	ctx := context.Background()
	rec.mu.Lock()
	defer rec.mu.Unlock()

	for _, c := range w.chains {
		got, err := st.Get(ctx, entitlements.SourceGoogle, c)
		if err != nil {
			t.Errorf("chain %s: %v", c, err)
			continue
		}
		applied := rec.applied[chainKey(entitlements.SourceGoogle, c)]
		if got.Version != int64(len(applied)) {
			t.Errorf("chain %s: version %d, applied events %d", c, got.Version, len(applied))
		}
		if got.UpdatedAt.Before(w.ownMax[c]) {
			t.Errorf("chain %s: lost update, updatedAt %s is older than its newest own event %s", c, got.UpdatedAt, w.ownMax[c])
		}
		var newest entitlements.Event
		for _, e := range applied {
			if !e.OccurredAt.Before(newest.OccurredAt) {
				newest = e
			}
		}
		if !got.UpdatedAt.Equal(newest.OccurredAt) || got.State != newest.State {
			t.Errorf("chain %s: final %s at %s, newest applied event is %s at %s", c, got.State, got.UpdatedAt, newest.State, newest.OccurredAt)
		}
	}

	// A chain whose own events were all older than the tombstone left by a
	// supersede holds only that userless tombstone, which ListByUser skips.
	wantListed := 0
	for _, c := range w.chains {
		for _, e := range rec.applied[chainKey(entitlements.SourceGoogle, c)] {
			if e.UserID != "" {
				wantListed++
				break
			}
		}
	}
	list, err := st.ListByUser(ctx, "u")
	if err != nil || len(list) != wantListed {
		t.Errorf("ListByUser = %d rows, %v; want %d (one per chain with an applied user event)", len(list), err, wantListed)
	}

	if cfg.ProcessedCount != nil {
		n, err := cfg.ProcessedCount(ctx)
		if err != nil || n != len(w.unique) {
			t.Errorf("processed events = %d, %v; want %d distinct", n, err, len(w.unique))
		}
	}
	// Every distinct event must have been recorded: redelivering it, and the
	// synthetic event that retires a superseded chain, is a duplicate.
	seen := map[string]bool{}
	for _, e := range w.events {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		redeliver := []entitlements.Event{e}
		if e.SupersedesTxID != "" {
			redeliver = append(redeliver, entitlements.Event{ID: e.ID + "#supersede", Source: e.Source, OriginalTxID: e.SupersedesTxID, State: entitlements.StateExpired, OccurredAt: e.OccurredAt})
		}
		for _, r := range redeliver {
			_, err := st.Apply(ctx, r, func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
				t.Errorf("fn called for already processed event %s", r.ID)
				return nil, nil
			})
			if !errors.Is(err, entitlements.ErrDuplicate) {
				t.Errorf("redelivery of %s = %v, want ErrDuplicate", r.ID, err)
			}
		}
	}
}
