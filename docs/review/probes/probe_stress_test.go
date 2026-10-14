//go:build probe

package entitlements_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/memory"
)

// recorder wraps a Store and records what fn actually produced.
type recorder struct {
	entitlements.Store
	mu       sync.Mutex
	fnCalls  map[string]int // event id -> fn invocations
	applied  map[string][]entitlements.Event
	appliedN map[string]int
	lastNext map[string]*entitlements.Entitlement
	errs     map[string]int
}

func newRecorder(s entitlements.Store) *recorder {
	return &recorder{Store: s, fnCalls: map[string]int{}, applied: map[string][]entitlements.Event{}, appliedN: map[string]int{}, errs: map[string]int{}}
}

func (r *recorder) Apply(ctx context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	var next *entitlements.Entitlement
	var fnErr error
	called := 0
	out, err := r.Store.Apply(ctx, ev, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		called++
		next, fnErr = fn(cur)
		return next, fnErr
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fnCalls[ev.ID] += called
	k := string(ev.Source) + ":" + ev.OriginalTxID
	if err == nil && next != nil && fnErr == nil {
		r.appliedN[k]++
		r.applied[k] = append(r.applied[k], ev)
	}
	if err != nil {
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe):
			r.errs["sqlstate "+pe.Code]++
		case errors.Is(err, entitlements.ErrDuplicate):
			r.errs["dup"]++
		case errors.Is(err, entitlements.ErrStale):
			r.errs["stale"]++
		default:
			r.errs[fmt.Sprintf("other: %v", err)]++
		}
	}
	return out, err
}

type workload struct {
	events []entitlements.Event
	chains []string
	ownMax map[string]time.Time
	uniq   map[string]bool // event ids incl. synthetic
}

// buildWorkload: K chains, own events at t=1..n minutes, plus crossing supersedes
// (chain i supersedes i+1 AND chain i+1 supersedes i), each duplicated dup times.
func buildWorkload(K, n, dup int, seed int64) workload {
	rnd := rand.New(rand.NewSource(seed))
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := workload{ownMax: map[string]time.Time{}, uniq: map[string]bool{}}
	for i := 0; i < K; i++ {
		w.chains = append(w.chains, fmt.Sprintf("tok%d", i))
	}
	states := []entitlements.State{entitlements.StateActive, entitlements.StateGrace, entitlements.StateBillingRetry, entitlements.StateActive}
	var evs []entitlements.Event
	for i, c := range w.chains {
		for j := 1; j <= n; j++ {
			at := base.Add(time.Duration(j)*time.Minute + time.Duration(i)*time.Second)
			e := entitlements.Event{ID: fmt.Sprintf("%s-e%d", c, j), Source: entitlements.SourceGoogle, UserID: "u", OriginalTxID: c,
				ProductID: "pro", State: states[j%len(states)], OccurredAt: at, ExpiresAt: at.Add(time.Hour)}
			if j%3 == 0 { // crossing supersedes: i->i+1 and (via its neighbour) i+1->i
				e.SupersedesTxID = w.chains[(i+1)%K]
			}
			evs = append(evs, e)
			if at.After(w.ownMax[c]) {
				w.ownMax[c] = at
			}
			w.uniq[e.ID] = true
			if e.SupersedesTxID != "" {
				w.uniq[e.ID+"#supersede"] = true
			}
		}
	}
	for d := 0; d < dup; d++ {
		evs = append(evs, append([]entitlements.Event(nil), evs[:K*n]...)...)
	}
	rnd.Shuffle(len(evs), func(a, b int) { evs[a], evs[b] = evs[b], evs[a] })
	w.events = evs
	return w
}

func runWorkload(t *testing.T, svc *entitlements.Service, w workload, goroutines int) (hung bool) {
	t.Helper()
	ch := make(chan entitlements.Event)
	var wg sync.WaitGroup
	var bad sync.Map
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range ch {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				_, err := svc.Process(ctx, ev)
				cancel()
				if err != nil && !errors.Is(err, entitlements.ErrDuplicate) && !errors.Is(err, entitlements.ErrStale) {
					bad.Store(err.Error(), true)
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
	case <-time.After(90 * time.Second):
		return true
	}
	bad.Range(func(k, _ any) bool { t.Errorf("unexpected Process error: %v", k); return true })
	return false
}

func checkInvariants(t *testing.T, store entitlements.Store, rec *recorder, w workload) {
	t.Helper()
	ctx := context.Background()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for id, n := range rec.fnCalls {
		if n > 1 {
			t.Errorf("event %s: fn invoked %d times (duplicate applied)", id, n)
		}
	}
	for _, c := range w.chains {
		got, err := store.Get(ctx, entitlements.SourceGoogle, c)
		if err != nil {
			t.Errorf("chain %s: %v", c, err)
			continue
		}
		k := "google:" + c
		if got.Version != int64(rec.appliedN[k]) {
			t.Errorf("chain %s: version %d != applied count %d", c, got.Version, rec.appliedN[k])
		}
		if got.UpdatedAt.Before(w.ownMax[c]) {
			t.Errorf("chain %s: LOST UPDATE updatedAt %s < newest own event %s", c, got.UpdatedAt, w.ownMax[c])
		}
		// last applied event (max OccurredAt among applied) must define state
		var best entitlements.Event
		for _, e := range rec.applied[k] {
			if e.OccurredAt.After(best.OccurredAt) {
				best = e
			}
		}
		if !got.UpdatedAt.Equal(best.OccurredAt) || got.State != best.State {
			t.Errorf("chain %s: final %s/%s != newest applied %s/%s", c, got.State, got.UpdatedAt, best.State, best.OccurredAt)
		}
		// applied events must be monotonic in OccurredAt per chain (order of application unknown) -> count only
	}
}

func TestStress_Memory(t *testing.T) {
	for round := 0; round < 30; round++ {
		store := memory.New()
		rec := newRecorder(store)
		svc := entitlements.NewService(rec)
		w := buildWorkload(6, 9, 3, int64(round))
		if runWorkload(t, svc, w, 64) {
			t.Fatal("HUNG (deadlock?)")
		}
		checkInvariants(t, store, rec, w)
		if t.Failed() {
			t.Logf("round %d errs=%v", round, rec.errs)
			return
		}
	}
}
