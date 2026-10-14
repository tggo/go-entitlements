// Package storetest is a conformance suite for entitlements.Store
// implementations. Run it from your store's tests:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) entitlements.Store {
//			return mystore.New(cleanDatabase(t))
//		})
//	}
//
// The suite encodes the numbered rules in the entitlements.Store doc. It only
// uses the standard library (t.Errorf and friends), so importing it adds no
// test-framework dependency to your module.
//
// Every subtest calls newStore for a fresh, empty store. Subtests run one
// after another and never call t.Parallel, so a factory may hand out one
// shared backend and reset it on each call. Concurrency is exercised inside
// the concurrency subtests with plain goroutines.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tggo/go-entitlements"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Run executes the whole suite against stores built by newStore.
func Run(t *testing.T, newStore func(t *testing.T) entitlements.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, st entitlements.Store)
	}{
		{"CreateGetAndNotFound", createGetAndNotFound},
		{"ZeroExpiresAtRoundTrips", zeroExpiresRoundTrips},
		{"SandboxRoundTrip", sandboxRoundTrip},
		{"FnSeesNilThenCurrent", fnSeesNilThenCurrent},
		{"ApplyReturnsSavedEntitlement", applyReturnsSaved},
		{"DuplicateEventIsRejectedWithoutCallingFn", duplicateRejected},
		{"DedupeIsPerSource", dedupePerSource},
		{"StaleIgnoredUnsupportedAreRecordedNotApplied", recordedNotApplied},
		{"OtherFnErrorRollsBackAndIsRetryable", otherErrorRollsBack},
		{"NilResultRecordsWithoutSaving", nilResultRecords},
		{"FnMayRunMoreThanOnce", fnMayRunMoreThanOnce},
		{"ListByUser", listByUser},
		{"ListByUserEmptyMatchesNothing", listByUserEmpty},
		{"ChainsAreIndependentAcrossSources", chainsIndependent},
		{"OutOfOrderEqualTimestampAndRefund", stateMachineRules},
		{"SupersedeAndTombstoneViaService", supersedeViaService},
		{"ConcurrentDeliverySameChain", concurrentSameChain},
		{"ConcurrentDuplicateSameID", concurrentDuplicateID},
		{"CancelledContext", cancelledContext},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, newStore(t))
		})
	}
}

func event(id string, state entitlements.State, at time.Time) entitlements.Event {
	return entitlements.Event{
		ID: id, Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "tx1", ProductID: "pro",
		State: state, ExpiresAt: at.Add(30 * 24 * time.Hour), OccurredAt: at, Raw: []byte(`{"id":"` + id + `"}`),
	}
}

// apply runs the state machine through the store, like Service does.
func apply(st entitlements.Store, e entitlements.Event) (*entitlements.Entitlement, error) {
	return st.Apply(context.Background(), e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return entitlements.Apply(cur, e)
	})
}

func mustApply(t *testing.T, st entitlements.Store, e entitlements.Event) *entitlements.Entitlement {
	t.Helper()
	got, err := apply(st, e)
	if err != nil {
		t.Fatalf("apply %s: %v", e.ID, err)
	}
	return got
}

func mustGet(t *testing.T, st entitlements.Store, source entitlements.Source, tx string) *entitlements.Entitlement {
	t.Helper()
	got, err := st.Get(context.Background(), source, tx)
	if err != nil {
		t.Fatalf("get %s/%s: %v", source, tx, err)
	}
	return got
}

func wantErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", what, err, want)
	}
}

func fnReturning(err error, called *int) func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
	return func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
		if called != nil {
			*called++
		}
		return nil, err
	}
}

func createGetAndNotFound(t *testing.T, st entitlements.Store) {
	got := mustApply(t, st, event("e1", entitlements.StateActive, t0))
	if got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
	e := mustGet(t, st, entitlements.SourceApple, "tx1")
	if e.State != entitlements.StateActive || e.UserID != "u1" || e.ProductID != "pro" || e.Source != entitlements.SourceApple || e.OriginalTxID != "tx1" {
		t.Errorf("stored entitlement = %+v", e)
	}
	if !e.UpdatedAt.Equal(t0) || !e.ExpiresAt.Equal(t0.Add(30*24*time.Hour)) {
		t.Errorf("times = updated %v expires %v", e.UpdatedAt, e.ExpiresAt)
	}
	_, err := st.Get(context.Background(), entitlements.SourceApple, "missing")
	wantErr(t, err, entitlements.ErrNotFound, "Get missing")
	_, err = st.Get(context.Background(), entitlements.SourceGoogle, "tx1")
	wantErr(t, err, entitlements.ErrNotFound, "Get same tx under another source")
}

func zeroExpiresRoundTrips(t *testing.T, st entitlements.Store) {
	e := event("e1", entitlements.StateActive, t0)
	e.ExpiresAt = time.Time{}
	mustApply(t, st, e)
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); !got.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero", got.ExpiresAt)
	}
}

func sandboxRoundTrip(t *testing.T, st entitlements.Store) {
	e := event("e1", entitlements.StateActive, t0)
	e.Sandbox = true
	if got := mustApply(t, st, e); !got.Sandbox {
		t.Error("Apply result lost Sandbox")
	}
	if !mustGet(t, st, entitlements.SourceApple, "tx1").Sandbox {
		t.Error("stored entitlement lost Sandbox")
	}
	mustApply(t, st, event("e2", entitlements.StateActive, t0.Add(time.Minute)))
	if mustGet(t, st, entitlements.SourceApple, "tx1").Sandbox {
		t.Error("Sandbox must follow the latest applied event")
	}
}

func fnSeesNilThenCurrent(t *testing.T, st entitlements.Store) {
	var seen []*entitlements.Entitlement
	record := func(e entitlements.Event) {
		_, err := st.Apply(context.Background(), e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
			seen = append(seen, cur)
			return entitlements.Apply(cur, e)
		})
		if err != nil {
			t.Fatalf("apply %s: %v", e.ID, err)
		}
	}
	record(event("e1", entitlements.StateActive, t0))
	first := len(seen)
	record(event("e2", entitlements.StateGrace, t0.Add(time.Minute)))
	if first == 0 || seen[0] != nil {
		t.Fatalf("fn must get nil for a new chain, got %v", seen)
	}
	last := seen[len(seen)-1]
	if last == nil || last.State != entitlements.StateActive || last.Version != 1 {
		t.Errorf("fn must get the current entitlement for an existing chain, got %+v", last)
	}
}

func applyReturnsSaved(t *testing.T, st entitlements.Store) {
	got := mustApply(t, st, event("e1", entitlements.StateActive, t0))
	if got.Source != entitlements.SourceApple || got.OriginalTxID != "tx1" || got.State != entitlements.StateActive {
		t.Errorf("returned entitlement = %+v", got)
	}
}

func duplicateRejected(t *testing.T, st entitlements.Store) {
	mustApply(t, st, event("e1", entitlements.StateActive, t0))
	called := 0
	_, err := st.Apply(context.Background(), event("e1", entitlements.StateActive, t0), fnReturning(nil, &called))
	wantErr(t, err, entitlements.ErrDuplicate, "redelivery")
	if called != 0 {
		t.Errorf("fn called %d times for a duplicate", called)
	}
	if v := mustGet(t, st, entitlements.SourceApple, "tx1").Version; v != 1 {
		t.Errorf("version = %d after duplicate, want 1", v)
	}
}

func dedupePerSource(t *testing.T, st entitlements.Store) {
	a := event("same-id", entitlements.StateActive, t0)
	g := a
	g.Source, g.OriginalTxID = entitlements.SourceGoogle, "tokG"
	mustApply(t, st, a)
	mustApply(t, st, g) // same ID, other source: not a duplicate
	_, err := apply(st, a)
	wantErr(t, err, entitlements.ErrDuplicate, "same ID, same source")
	_, err = apply(st, g)
	wantErr(t, err, entitlements.ErrDuplicate, "same ID, same source (google)")
}

func recordedNotApplied(t *testing.T, st entitlements.Store) {
	mustApply(t, st, event("e0", entitlements.StateActive, t0))
	for _, sentinel := range []error{entitlements.ErrStale, entitlements.ErrIgnored, entitlements.ErrUnsupported} {
		id := "e-" + sentinel.Error()
		wrapped := fmt.Errorf("because: %w", sentinel)
		cur, err := st.Apply(context.Background(), event(id, entitlements.StateExpired, t0), fnReturning(wrapped, nil))
		wantErr(t, err, sentinel, "fn error is returned")
		if cur == nil || cur.State != entitlements.StateActive || cur.Version != 1 {
			t.Errorf("%v: current entitlement not returned alongside the error: %+v", sentinel, cur)
		}
		_, err = apply(st, event(id, entitlements.StateExpired, t0))
		wantErr(t, err, entitlements.ErrDuplicate, "recorded event, redelivered")
	}
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.Version != 1 || got.State != entitlements.StateActive {
		t.Errorf("entitlement changed: %+v", got)
	}
}

func otherErrorRollsBack(t *testing.T, st entitlements.Store) {
	boom := errors.New("boom")
	_, err := st.Apply(context.Background(), event("e1", entitlements.StateActive, t0), fnReturning(boom, nil))
	wantErr(t, err, boom, "fn error is returned")
	_, err = st.Get(context.Background(), entitlements.SourceApple, "tx1")
	wantErr(t, err, entitlements.ErrNotFound, "nothing persisted")
	mustApply(t, st, event("e1", entitlements.StateActive, t0)) // same ID is retryable
}

func nilResultRecords(t *testing.T, st entitlements.Store) {
	cur, err := st.Apply(context.Background(), event("e1", entitlements.StateActive, t0), fnReturning(nil, nil))
	if err != nil || cur != nil {
		t.Fatalf("Apply = %v, %v; want nil, nil", cur, err)
	}
	_, err = st.Get(context.Background(), entitlements.SourceApple, "tx1")
	wantErr(t, err, entitlements.ErrNotFound, "nothing saved")
	_, err = apply(st, event("e1", entitlements.StateActive, t0))
	wantErr(t, err, entitlements.ErrDuplicate, "event is recorded")

	// With an existing chain the current entitlement comes back unchanged.
	mustApply(t, st, event("e2", entitlements.StateActive, t0))
	cur, err = st.Apply(context.Background(), event("e3", entitlements.StateActive, t0), fnReturning(nil, nil))
	if err != nil || cur == nil || cur.Version != 1 {
		t.Errorf("Apply on existing chain = %+v, %v", cur, err)
	}
}

// fn may be called more than once (a store may retry its transaction), so the
// outcome must depend only on fn's last return value, not on the call count.
func fnMayRunMoreThanOnce(t *testing.T, st entitlements.Store) {
	calls := 0
	e := event("e1", entitlements.StateActive, t0)
	_, err := st.Apply(context.Background(), e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		calls++
		return entitlements.Apply(cur, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 1 {
		t.Fatalf("fn called %d times", calls)
	}
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.Version != 1 {
		t.Errorf("version = %d after %d fn calls, want 1", got.Version, calls)
	}
}

func listByUser(t *testing.T, st entitlements.Store) {
	for i, c := range []struct {
		src  entitlements.Source
		tx   string
		user string
	}{
		{entitlements.SourceGoogle, "a", "u1"},
		{entitlements.SourceApple, "b", "u1"},
		{entitlements.SourceApple, "a", "u1"},
		{entitlements.SourceApple, "z", "u2"},
	} {
		e := event(fmt.Sprint("e", i), entitlements.StateActive, t0)
		e.Source, e.OriginalTxID, e.UserID = c.src, c.tx, c.user
		mustApply(t, st, e)
	}
	list, err := st.ListByUser(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range list {
		got = append(got, string(e.Source)+"/"+e.OriginalTxID)
	}
	want := []string{"apple/a", "apple/b", "google/a"} // ordered by (source, original tx id)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ListByUser = %v, want %v", got, want)
	}
	none, err := st.ListByUser(context.Background(), "nobody")
	if err != nil || len(none) != 0 {
		t.Errorf("ListByUser(nobody) = %v, %v", none, err)
	}
}

func listByUserEmpty(t *testing.T, st entitlements.Store) {
	e := event("e1", entitlements.StateActive, t0)
	e.UserID = ""
	mustApply(t, st, e)
	list, err := st.ListByUser(context.Background(), "")
	if err != nil || len(list) != 0 {
		t.Errorf("ListByUser(\"\") = %v, %v; want empty", list, err)
	}
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.UserID != "" {
		t.Errorf("UserID = %q", got.UserID)
	}
}

func chainsIndependent(t *testing.T, st entitlements.Store) {
	a := event("e1", entitlements.StateActive, t0)
	g := event("e2", entitlements.StateExpired, t0)
	g.Source = entitlements.SourceGoogle // same OriginalTxID, other source
	mustApply(t, st, a)
	mustApply(t, st, g)
	if mustGet(t, st, entitlements.SourceApple, "tx1").State != entitlements.StateActive ||
		mustGet(t, st, entitlements.SourceGoogle, "tx1").State != entitlements.StateExpired {
		t.Error("chains with the same OriginalTxID under different sources must not share state")
	}
}

// stateMachineRules checks that the store hands fn the true current state, by
// replaying ordering cases through entitlements.Apply.
func stateMachineRules(t *testing.T, st entitlements.Store) {
	mustApply(t, st, event("e1", entitlements.StateActive, t0.Add(time.Hour)))
	_, err := apply(st, event("e2", entitlements.StateExpired, t0))
	wantErr(t, err, entitlements.ErrStale, "older event")
	if e := mustGet(t, st, entitlements.SourceApple, "tx1"); e.State != entitlements.StateActive || e.Version != 1 {
		t.Errorf("stale event changed state: %+v", e)
	}
	_, err = apply(st, event("e2", entitlements.StateExpired, t0))
	wantErr(t, err, entitlements.ErrDuplicate, "stale event is recorded")

	mustApply(t, st, event("e3", entitlements.StateGrace, t0.Add(time.Hour))) // equal timestamp is applied
	if e := mustGet(t, st, entitlements.SourceApple, "tx1"); e.State != entitlements.StateGrace || e.Version != 2 {
		t.Errorf("equal-timestamp event: %+v", e)
	}

	mustApply(t, st, event("e4", entitlements.StateRefunded, t0.Add(3*time.Hour)))
	_, err = apply(st, event("e5", entitlements.StateActive, t0.Add(2*time.Hour)))
	wantErr(t, err, entitlements.ErrStale, "renew that happened before the refund")
	_, err = apply(st, event("e6", entitlements.StateBillingRetry, t0.Add(4*time.Hour)))
	wantErr(t, err, entitlements.ErrStale, "renewal failure after refund")
	if e := mustGet(t, st, entitlements.SourceApple, "tx1"); e.State != entitlements.StateRefunded {
		t.Errorf("refund undone: %+v", e)
	}
}

func supersedeViaService(t *testing.T, st entitlements.Store) {
	ctx := context.Background()
	svc := entitlements.NewService(st, entitlements.WithTimeFn(func() time.Time { return t0.Add(time.Minute) }))

	old := event("e1", entitlements.StateActive, t0)
	old.Source, old.OriginalTxID = entitlements.SourceGoogle, "tokA"
	if _, err := svc.Process(ctx, old); err != nil {
		t.Fatal(err)
	}
	nw := event("e2", entitlements.StateActive, t0.Add(time.Minute))
	nw.Source, nw.OriginalTxID, nw.SupersedesTxID = entitlements.SourceGoogle, "tokB", "tokA"
	if _, err := svc.Process(ctx, nw); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Process(ctx, nw); !errors.Is(err, entitlements.ErrDuplicate) {
		t.Errorf("redelivery = %v, want ErrDuplicate", err)
	}
	if mustGet(t, st, entitlements.SourceGoogle, "tokA").State != entitlements.StateExpired ||
		mustGet(t, st, entitlements.SourceGoogle, "tokB").State != entitlements.StateActive {
		t.Error("old chain must be expired, new chain active")
	}

	// Old chain never seen: it gets an expired tombstone, so a late, older
	// event for it is stale instead of resurrecting it.
	ghost := event("e3", entitlements.StateActive, t0.Add(2*time.Minute))
	ghost.Source, ghost.OriginalTxID, ghost.SupersedesTxID = entitlements.SourceGoogle, "tokC", "tokGhost"
	if _, err := svc.Process(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, st, entitlements.SourceGoogle, "tokGhost").State != entitlements.StateExpired {
		t.Error("unseen old chain must be tombstoned as expired")
	}
	late := event("e4", entitlements.StateActive, t0)
	late.Source, late.OriginalTxID = entitlements.SourceGoogle, "tokGhost"
	if _, err := svc.Process(ctx, late); !errors.Is(err, entitlements.ErrStale) {
		t.Errorf("late event for tombstoned chain = %v, want ErrStale", err)
	}
}

const workers = 40

func concurrentSameChain(t *testing.T, st entitlements.Store) {
	results := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// the first writers race to create the row
			_, results[i] = apply(st, event(fmt.Sprintf("e%02d", i), entitlements.StateActive, t0.Add(time.Duration(i)*time.Second)))
		}()
	}
	wg.Wait()

	var applied int
	for _, err := range results {
		switch {
		case err == nil:
			applied++
		case errors.Is(err, entitlements.ErrStale):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	e := mustGet(t, st, entitlements.SourceApple, "tx1")
	if want := t0.Add((workers - 1) * time.Second); !e.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want the newest event %v", e.UpdatedAt, want)
	}
	if e.Version != int64(applied) {
		t.Errorf("Version = %d, applied events = %d", e.Version, applied)
	}
	list, err := st.ListByUser(context.Background(), "u1")
	if err != nil || len(list) != 1 {
		t.Errorf("ListByUser = %d rows, %v; want exactly one chain row", len(list), err)
	}
}

func concurrentDuplicateID(t *testing.T, st entitlements.Store) {
	const n = 20
	results := make([]error, n)
	e := event("same", entitlements.StateActive, t0)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = apply(st, e)
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
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || dup != n-1 {
		t.Errorf("applied %d, duplicates %d; want 1 and %d", ok, dup, n-1)
	}
	if v := mustGet(t, st, entitlements.SourceApple, "tx1").Version; v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
}

func cancelledContext(t *testing.T, st entitlements.Store) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := event("e1", entitlements.StateActive, t0)
	_, err := st.Apply(ctx, e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return entitlements.Apply(cur, e)
	})
	if err == nil {
		t.Error("Apply with a cancelled context must fail")
	}
	if _, err = st.Get(ctx, entitlements.SourceApple, "tx1"); err == nil {
		t.Error("Get with a cancelled context must fail")
	}
	if _, err = st.ListByUser(ctx, "u1"); err == nil {
		t.Error("ListByUser with a cancelled context must fail")
	}
	// A cancelled Apply must not have consumed the event ID.
	mustApply(t, st, e)
}
