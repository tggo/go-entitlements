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
	"slices"
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
		{"EnvironmentRoundTrip", environmentRoundTrip},
		{"FnSeesNilThenCurrent", fnSeesNilThenCurrent},
		{"ApplyReturnsSavedEntitlement", applyReturnsSaved},
		{"RenewalPersistsEveryChangedField", renewalPersists},
		{"DuplicateEventIsRejectedWithoutCallingFn", duplicateRejected},
		{"DedupeIsPerSource", dedupePerSource},
		{"StaleIgnoredUnsupportedAreRecordedNotApplied", recordedNotApplied},
		{"OtherFnErrorRollsBackAndIsRetryable", otherErrorRollsBack},
		{"NilResultRecordsWithoutSaving", nilResultRecords},
		{"FnMayRunMoreThanOnce", fnMayRunMoreThanOnce},
		{"RefundOfOlderTransactionBeforeItsRenewal", refundOldBeforeRenewal},
		{"RenewalBeforeRefundOfOlderTransaction", renewalBeforeRefundOld},
		{"RefundOfLatestTransactionThenReversal", refundLatestThenReversal},
		{"RefundedTransactionsRoundTripAndDuplicates", refundedTransactionsRoundTrip},
		{"AutoRenewRoundTrip", autoRenewRoundTrip},
		{"DeleteUser", deleteUser},
		{"LinkUserThroughService", linkUser},
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

func environmentRoundTrip(t *testing.T, st entitlements.Store) {
	e := event("e1", entitlements.StateActive, t0)
	e.Environment = entitlements.EnvironmentSandbox
	if got := mustApply(t, st, e); got.Environment != entitlements.EnvironmentSandbox {
		t.Errorf("Apply result Environment = %q", got.Environment)
	}
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.Environment != entitlements.EnvironmentSandbox || !got.Sandbox() {
		t.Errorf("stored Environment = %q", got.Environment)
	}
	mustApply(t, st, event("e2", entitlements.StateActive, t0.Add(time.Minute)))
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.Sandbox() {
		t.Errorf("Environment must follow the latest applied event, got %q", got.Environment)
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
	e := event("e1", entitlements.StateActive, t0)
	e.Environment = entitlements.EnvironmentSandbox
	got := mustApply(t, st, e)
	if got.Source != entitlements.SourceApple || got.OriginalTxID != "tx1" || got.State != entitlements.StateActive {
		t.Errorf("returned entitlement = %+v", got)
	}
	// what the store persisted must equal what Apply returned
	stored := mustGet(t, st, entitlements.SourceApple, "tx1")
	if !sameEntitlement(*got, *stored) {
		t.Errorf("Apply returned %+v but the store holds %+v", *got, *stored)
	}
	if stored.UpdatedAt.Location() != time.UTC {
		t.Errorf("UpdatedAt location = %v, want UTC", stored.UpdatedAt.Location())
	}
}

func sameEntitlement(a, b entitlements.Entitlement) bool {
	return a.UserID == b.UserID && a.ProductID == b.ProductID && a.OriginalTxID == b.OriginalTxID &&
		a.Source == b.Source && a.State == b.State && a.ExpiresAt.Equal(b.ExpiresAt) &&
		a.UpdatedAt.Equal(b.UpdatedAt) && a.Version == b.Version && a.Environment == b.Environment
}

// renewalPersists applies a second event that changes every mutable column
// and reads the row back: the UPDATE path must store all of them.
func renewalPersists(t *testing.T, st entitlements.Store) {
	first := event("e1", entitlements.StateActive, t0)
	first.UserID, first.ProductID, first.Environment = "", "pro-monthly", entitlements.EnvironmentSandbox
	first.ExpiresAt = t0.Add(24 * time.Hour)
	mustApply(t, st, first)

	renewal := event("e2", entitlements.StateGrace, t0.Add(time.Hour))
	renewal.UserID, renewal.ProductID, renewal.Environment = "u7", "pro-yearly", entitlements.EnvironmentProduction
	renewal.ExpiresAt = t0.Add(400 * 24 * time.Hour)
	mustApply(t, st, renewal)

	got := mustGet(t, st, entitlements.SourceApple, "tx1")
	want := entitlements.Entitlement{
		UserID: "u7", ProductID: "pro-yearly", OriginalTxID: "tx1", Source: entitlements.SourceApple,
		State: entitlements.StateGrace, ExpiresAt: t0.Add(400 * 24 * time.Hour), UpdatedAt: t0.Add(time.Hour),
		Version: 2, Environment: entitlements.EnvironmentProduction,
	}
	if !sameEntitlement(*got, want) {
		t.Errorf("stored after renewal = %+v, want %+v", *got, want)
	}
	if list, err := st.ListByUser(context.Background(), "u7"); err != nil || len(list) != 1 {
		t.Errorf("ListByUser(u7) = %d rows, %v; the new user id must be persisted", len(list), err)
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

const (
	tx1 = "tx-period-1"
	tx2 = "tx-period-2"
)

var (
	exp1 = t0.Add(30 * 24 * time.Hour)
	exp2 = t0.Add(60 * 24 * time.Hour)
)

func period(id string, state entitlements.State, signed time.Time, tx string, exp time.Time) entitlements.Event {
	e := event(id, state, signed)
	e.TransactionID, e.ExpiresAt = tx, exp
	return e
}

func wantChain(t *testing.T, st entitlements.Store, state entitlements.State, exp time.Time, version int64, refunded ...string) {
	t.Helper()
	got := mustGet(t, st, entitlements.SourceApple, "tx1")
	if got.State != state || !got.ExpiresAt.Equal(exp) || got.Version != version || !slices.Equal(got.RefundedTransactions, refunded) {
		t.Errorf("stored chain = state %s expires %v version %d refunded %v; want %s %v %d %v",
			got.State, got.ExpiresAt, got.Version, got.RefundedTransactions, state, exp, version, refunded)
	}
}

// The REFUND of period 1 is delivered before the renewal that replaced it and
// is signed after it: the renewal must still apply.
func refundOldBeforeRenewal(t *testing.T, st entitlements.Store) {
	mustApply(t, st, period("sub", entitlements.StateActive, t0, tx1, exp1))
	mustApply(t, st, period("refund-1", entitlements.StateRefunded, t0.Add(2*time.Hour), tx1, exp1))
	wantChain(t, st, entitlements.StateRefunded, exp1, 2, tx1)

	mustApply(t, st, period("renew-2", entitlements.StateActive, t0.Add(time.Hour), tx2, exp2))
	wantChain(t, st, entitlements.StateActive, exp2, 3, tx1)

	// redelivery of any of them changes nothing
	for _, id := range []string{"sub", "refund-1", "renew-2"} {
		_, err := apply(st, period(id, entitlements.StateActive, t0, tx1, exp1))
		wantErr(t, err, entitlements.ErrDuplicate, id)
	}
	wantChain(t, st, entitlements.StateActive, exp2, 3, tx1)
}

func renewalBeforeRefundOld(t *testing.T, st entitlements.Store) {
	mustApply(t, st, period("sub", entitlements.StateActive, t0, tx1, exp1))
	mustApply(t, st, period("renew-2", entitlements.StateActive, t0.Add(time.Hour), tx2, exp2))
	mustApply(t, st, period("refund-1", entitlements.StateRefunded, t0.Add(2*time.Hour), tx1, exp1))
	wantChain(t, st, entitlements.StateActive, exp2, 3, tx1)
}

func refundLatestThenReversal(t *testing.T, st entitlements.Store) {
	mustApply(t, st, period("sub", entitlements.StateActive, t0, tx1, exp1))
	mustApply(t, st, period("renew-2", entitlements.StateActive, t0.Add(time.Hour), tx2, exp2))
	mustApply(t, st, period("refund-2", entitlements.StateRefunded, t0.Add(2*time.Hour), tx2, exp2))
	wantChain(t, st, entitlements.StateRefunded, exp2, 3, tx2)

	// a late renewal event of the refunded period is refused
	_, err := apply(st, period("renew-2-late", entitlements.StateActive, t0.Add(3*time.Hour), tx2, exp2))
	wantErr(t, err, entitlements.ErrStale, "renewal of the refunded transaction")
	wantChain(t, st, entitlements.StateRefunded, exp2, 3, tx2)

	reversal := period("reversed", entitlements.StateActive, t0.Add(4*time.Hour), tx2, exp2)
	reversal.Restores = true
	mustApply(t, st, reversal)
	wantChain(t, st, entitlements.StateActive, exp2, 4)
}

func refundedTransactionsRoundTrip(t *testing.T, st entitlements.Store) {
	mustApply(t, st, period("sub", entitlements.StateActive, t0, tx1, exp1))
	got := mustApply(t, st, period("refund-a", entitlements.StateRefunded, t0.Add(time.Hour), "tx-a", exp1.Add(-24*time.Hour)))
	if !slices.Equal(got.RefundedTransactions, []string{"tx-a"}) {
		t.Errorf("Apply returned RefundedTransactions %v", got.RefundedTransactions)
	}
	// the stored copy must not alias what Apply returned
	got.RefundedTransactions[0] = "tampered"
	wantChain(t, st, entitlements.StateActive, exp1, 2, "tx-a")
	mustApply(t, st, period("refund-b", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx-b", exp1.Add(-48*time.Hour)))
	wantChain(t, st, entitlements.StateActive, exp1, 3, "tx-a", "tx-b")
	// the same transaction refunded twice is listed once
	mustApply(t, st, period("refund-b2", entitlements.StateRefunded, t0.Add(3*time.Hour), "tx-b", exp1.Add(-48*time.Hour)))
	wantChain(t, st, entitlements.StateActive, exp1, 4, "tx-a", "tx-b")
}

func autoRenewRoundTrip(t *testing.T, st entitlements.Store) {
	yes, no := true, false
	mustApply(t, st, event("e1", entitlements.StateActive, t0))
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.AutoRenew != nil {
		t.Errorf("AutoRenew = %v, want nil (never reported)", *got.AutoRenew)
	}
	e := event("e2", entitlements.StateActive, t0.Add(time.Minute))
	e.AutoRenew = &yes
	mustApply(t, st, e)
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.AutoRenew == nil || !*got.AutoRenew {
		t.Errorf("AutoRenew = %v, want true", got.AutoRenew)
	}
	e = event("e3", entitlements.StateActive, t0.Add(2*time.Minute))
	e.AutoRenew = &no
	mustApply(t, st, e)
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.AutoRenew == nil || *got.AutoRenew {
		t.Errorf("AutoRenew = %v, want false", got.AutoRenew)
	}
	mustApply(t, st, event("e4", entitlements.StateActive, t0.Add(3*time.Minute))) // says nothing
	if got := mustGet(t, st, entitlements.SourceApple, "tx1"); got.AutoRenew == nil || *got.AutoRenew {
		t.Errorf("an event without AutoRenew changed it to %v", got.AutoRenew)
	}
	// the stored value does not alias the returned one
	got := mustGet(t, st, entitlements.SourceApple, "tx1")
	*got.AutoRenew = true
	if again := mustGet(t, st, entitlements.SourceApple, "tx1"); *again.AutoRenew {
		t.Error("mutating a returned entitlement changed the stored one")
	}
}

func deleteUser(t *testing.T, st entitlements.Store) {
	ctx := context.Background()
	chains := []struct {
		src  entitlements.Source
		tx   string
		user string
	}{
		{entitlements.SourceApple, "a1", "u1"}, {entitlements.SourceGoogle, "g1", "u1"},
		{entitlements.SourceApple, "b1", "u2"}, {entitlements.SourceApple, "n1", ""},
	}
	for i, c := range chains {
		e := event(fmt.Sprint("e", i), entitlements.StateActive, t0)
		e.Source, e.OriginalTxID, e.UserID = c.src, c.tx, c.user
		mustApply(t, st, e)
	}
	n, err := st.DeleteUser(ctx, "u1")
	if err != nil || n != 2 {
		t.Fatalf("DeleteUser(u1) = %d, %v; want 2, nil", n, err)
	}
	for _, c := range chains[:2] {
		_, err := st.Get(ctx, c.src, c.tx)
		wantErr(t, err, entitlements.ErrNotFound, "deleted chain "+c.tx)
	}
	if list, err := st.ListByUser(ctx, "u1"); err != nil || len(list) != 0 {
		t.Errorf("ListByUser(u1) = %v, %v", list, err)
	}
	mustGet(t, st, entitlements.SourceApple, "b1")
	mustGet(t, st, entitlements.SourceApple, "n1")

	// processed event IDs stay: a redelivery is a duplicate and cannot recreate the chain
	first := event("e0", entitlements.StateActive, t0)
	first.OriginalTxID, first.UserID = "a1", "u1"
	_, err = apply(st, first)
	wantErr(t, err, entitlements.ErrDuplicate, "redelivery after DeleteUser")

	if n, err = st.DeleteUser(ctx, "u1"); err != nil || n != 0 {
		t.Errorf("second DeleteUser = %d, %v; want 0, nil", n, err)
	}
	if n, err = st.DeleteUser(ctx, ""); err != nil || n != 0 {
		t.Errorf("DeleteUser(\"\") = %d, %v; want 0, nil", n, err)
	}
	mustGet(t, st, entitlements.SourceApple, "n1") // the unattributed chain survived the empty id
}

// LinkUser relies on Apply being atomic and on rolling back a failing callback.
func linkUser(t *testing.T, st entitlements.Store) {
	ctx := context.Background()
	svc := entitlements.NewService(st, entitlements.WithTimeFn(func() time.Time { return t0 }))
	e := event("e1", entitlements.StateActive, t0)
	e.UserID = ""
	if _, err := svc.Process(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
	if err != nil || got.UserID != "u5" || got.Version != 2 {
		t.Fatalf("LinkUser = %+v, %v", got, err)
	}
	if stored := mustGet(t, st, entitlements.SourceApple, "tx1"); stored.UserID != "u5" || stored.Version != 2 {
		t.Errorf("stored after link = %+v", stored)
	}
	if again, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5"); err != nil || again.Version != 2 {
		t.Errorf("repeated link = %+v, %v; want no change", again, err)
	}
	_, err = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "intruder")
	wantErr(t, err, entitlements.ErrUserConflict, "linking another user")
	_, err = svc.LinkUser(ctx, entitlements.SourceApple, "missing", "u5")
	wantErr(t, err, entitlements.ErrNotFound, "unknown chain")
	if list, err := st.ListByUser(ctx, "u5"); err != nil || len(list) != 1 {
		t.Errorf("ListByUser(u5) = %d rows, %v", len(list), err)
	}
	if stored := mustGet(t, st, entitlements.SourceApple, "tx1"); stored.UserID != "u5" || stored.Version != 2 {
		t.Errorf("failed links changed the chain: %+v", stored)
	}

	// Concurrent links of two users to unowned chains: Apply is atomic, so on
	// every chain exactly one user wins and every attempt by the other conflicts.
	const chains, perChain = 6, 16
	for c := range chains {
		e := event(fmt.Sprint("race-", c), entitlements.StateActive, t0)
		e.OriginalTxID, e.UserID = fmt.Sprint("tx-race-", c), ""
		if _, err := svc.Process(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	results := make([][]error, chains)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for c := range chains {
		results[c] = make([]error, perChain)
		for i := range perChain {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, results[c][i] = svc.LinkUser(ctx, entitlements.SourceApple, fmt.Sprint("tx-race-", c), []string{"ua", "ub"}[i%2])
			}()
		}
	}
	close(start)
	wg.Wait()
	for c := range chains {
		conflicts := 0
		for _, err := range results[c] {
			switch {
			case err == nil:
			case errors.Is(err, entitlements.ErrUserConflict):
				conflicts++
			default:
				t.Errorf("unexpected link error: %v", err)
			}
		}
		if conflicts != perChain/2 {
			t.Errorf("chain %d: %d conflicts, want %d: two users must not both own it", c, conflicts, perChain/2)
		}
		if raced := mustGet(t, st, entitlements.SourceApple, fmt.Sprint("tx-race-", c)); raced.Version != 2 {
			t.Errorf("chain %d: version %d, want 2 (linked once)", c, raced.Version)
		}
	}

	// a later event without a user, and one naming another user, keep the owner
	nu := event("e2", entitlements.StateGrace, t0.Add(time.Minute))
	nu.UserID = ""
	mustApply(t, st, nu)
	other := event("e3", entitlements.StateActive, t0.Add(2*time.Minute))
	other.UserID = "someone-else"
	mustApply(t, st, other)
	if stored := mustGet(t, st, entitlements.SourceApple, "tx1"); stored.UserID != "u5" {
		t.Errorf("owner = %q, want u5", stored.UserID)
	}
}
