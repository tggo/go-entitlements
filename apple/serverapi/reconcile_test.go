package serverapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/store/memory"
)

// snapshotAt is the signedDate of the status fixtures (2026-10-10 12:00 UTC).
var snapshotAt = time.UnixMilli(1791633600000).UTC()

func newService() (*entitlements.Service, *memory.Store) {
	st := memory.New()
	return entitlements.NewService(st, entitlements.WithAllowSandbox()), st
}

// reconcileFixtures serves the fixtures as one Get All Subscription Statuses
// response and runs Reconcile against a real Client.
func reconcileFixtures(t *testing.T, svc Applier, names ...string) ([]Reconciled, error) {
	t.Helper()
	f := newFakeApple(t)
	var items []map[string]any
	for _, n := range names {
		items = append(items, loadStatus(t, n).wireItem(t, f.ca))
	}
	f.respond(ok(statusResponse("Sandbox", items)))
	c, _ := f.client()
	return Reconcile(ctx, c, svc, origTx)
}

func TestEventFromStatus_States(t *testing.T) {
	ms := func(v int64) time.Time { return time.UnixMilli(v).UTC() }
	tests := []struct {
		fixture  string
		state    entitlements.State
		expires  time.Time
		restores bool
	}{
		{"status_active", entitlements.StateActive, ms(1794268800000), true},
		{"status_expired", entitlements.StateExpired, ms(1790812800000), true},
		{"status_billing_retry", entitlements.StateBillingRetry, ms(1791158400000), true},
		{"status_grace", entitlements.StateGrace, ms(1791849600000), true}, // gracePeriodExpiresDate, later than expiresDate
		{"status_refunded", entitlements.StateRefunded, ms(1794268800000), false},
		{"status_family_revoked", entitlements.StateRevoked, ms(1794268800000), false},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			f := newFakeApple(t)
			fx := loadStatus(t, tc.fixture)
			f.respond(ok(statusResponse("Sandbox", []map[string]any{fx.wireItem(t, f.ca)})))
			c, _ := f.client()
			st, err := c.GetAllSubscriptionStatuses(ctx, origTx)
			require.NoError(t, err)

			ev, err := EventFromStatus(st.Groups[0].Subscriptions[0])
			require.NoError(t, err)
			assert.Equal(t, tc.state, ev.State)
			assert.Equal(t, tc.expires, ev.ExpiresAt)
			assert.Equal(t, tc.restores, ev.Restores)
			assert.Equal(t, snapshotAt, ev.OccurredAt, "ordered by Apple's signedDate, not the local clock")
			assert.Equal(t, entitlements.SourceApple, ev.Source)
			assert.Equal(t, origTx, ev.OriginalTxID)
			assert.Equal(t, "2000000000000002", ev.TransactionID)
			assert.Equal(t, "com.example.app.monthly", ev.ProductID)
			assert.Equal(t, "7e3fb20b-4cdb-47cc-936d-99d65f608138", ev.UserID)
			assert.Equal(t, entitlements.EnvironmentSandbox, ev.Environment)
			assert.NotEmpty(t, ev.Raw)
			assert.False(t, ev.NoRevive)
			assert.Regexp(t, `^reconcile-[0-9a-f]{32}$`, ev.ID)

			again, err := EventFromStatus(st.Groups[0].Subscriptions[0])
			require.NoError(t, err)
			assert.Equal(t, ev.ID, again.ID, "the ID is deterministic")
		})
	}
}

func TestEventFromStatus_Refusals(t *testing.T) {
	tests := []struct {
		fixture string
		is      error
	}{
		{"status_active_no_expiry", apple.ErrNoExpiry},
		{"status_grace_no_date", apple.ErrNoExpiry},
		{"status_unknown", ErrUnknownStatus},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			f := newFakeApple(t)
			f.respond(ok(statusResponse("Sandbox", []map[string]any{loadStatus(t, tc.fixture).wireItem(t, f.ca)})))
			c, _ := f.client()
			st, err := c.GetAllSubscriptionStatuses(ctx, origTx)
			require.NoError(t, err)
			_, err = EventFromStatus(st.Groups[0].Subscriptions[0])
			assert.ErrorIs(t, err, tc.is)
			assert.ErrorIs(t, err, entitlements.ErrUnsupported)
		})
	}
	t.Run("no signedDate cannot be ordered", func(t *testing.T) {
		_, err := EventFromStatus(SubscriptionStatus{
			OriginalTransactionID: origTx, Status: StatusActive,
			Transaction: apple.JWSTransactionDecodedPayload{ExpiresDate: 1794268800000},
		})
		assert.ErrorIs(t, err, ErrNoSignedDate)
	})
}

func TestEventFromStatus_IDChangesWithTheSnapshot(t *testing.T) {
	base := SubscriptionStatus{
		OriginalTransactionID: origTx, Status: StatusActive,
		Transaction: apple.JWSTransactionDecodedPayload{TransactionID: "t1", ExpiresDate: 1794268800000, SignedDate: 1791633600000},
	}
	id := func(s SubscriptionStatus) string {
		ev, err := EventFromStatus(s)
		require.NoError(t, err)
		return ev.ID
	}
	ref := id(base)
	mut := map[string]func(*SubscriptionStatus){
		"transaction": func(s *SubscriptionStatus) { s.Transaction.TransactionID = "t2" },
		"status":      func(s *SubscriptionStatus) { s.Status = StatusExpired },
		"expiry":      func(s *SubscriptionStatus) { s.Transaction.ExpiresDate++ },
		"revocation":  func(s *SubscriptionStatus) { s.Transaction.RevocationDate = 5 },
		"signedDate":  func(s *SubscriptionStatus) { s.Transaction.SignedDate++ },
		"chain":       func(s *SubscriptionStatus) { s.OriginalTransactionID = "other" },
		"grace": func(s *SubscriptionStatus) {
			s.Renewal = &apple.JWSRenewalInfoDecodedPayload{GracePeriodExpiresDate: 9}
		},
	}
	for name, m := range mut {
		s := base
		m(&s)
		assert.NotEqual(t, ref, id(s), "%s must change the ID: a later state that happens to look like an earlier one must not be swallowed as a duplicate", name)
	}
}

// The reason the package exists: a webhook was lost, Apple knows the truth.
func TestReconcile_RepairsALostWebhook(t *testing.T) {
	svc, _ := newService()

	res, err := reconcileFixtures(t, svc, "status_active")
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, OutcomeApplied, res[0].Outcome)
	require.NotNil(t, res[0].Entitlement)

	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, ent.State)
	assert.Equal(t, time.UnixMilli(1794268800000).UTC(), ent.ExpiresAt)
	assert.Equal(t, "com.example.app.monthly", ent.ProductID)
	assert.Equal(t, snapshotAt, ent.UpdatedAt)

	ok, err := svc.IsActive(ctx, "7e3fb20b-4cdb-47cc-936d-99d65f608138", "com.example.app.monthly")
	require.NoError(t, err)
	assert.True(t, ok, "the user has access again")
}

func TestReconcile_RepairsAMissedExpiry(t *testing.T) {
	svc, _ := newService()
	_, err := svc.Process(ctx, entitlements.Event{
		ID: "uuid-subscribed", Source: entitlements.SourceApple, OriginalTxID: origTx, UserID: "u1",
		ProductID: "com.example.app.monthly", State: entitlements.StateActive,
		ExpiresAt: time.UnixMilli(1790812800000).UTC(), OccurredAt: snapshotAt.Add(-24 * time.Hour),
		Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)

	res, err := reconcileFixtures(t, svc, "status_expired")
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, res[0].Outcome)
	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ent.State)
}

func TestReconcile_RepeatedRunsWriteNothing(t *testing.T) {
	svc, _ := newService()
	for i := range 3 {
		res, err := reconcileFixtures(t, svc, "status_active")
		require.NoError(t, err)
		want := OutcomeUnchanged
		if i == 0 {
			want = OutcomeApplied
		}
		assert.Equal(t, want, res[0].Outcome, "run %d", i)
	}
	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, ent.Version, "no churn")
}

func TestReconcile_SameSnapshotDeliveredTwiceIsADuplicate(t *testing.T) {
	svc, _ := newService()
	f := newFakeApple(t)
	f.respond(ok(statusResponse("Sandbox", []map[string]any{loadStatus(t, "status_active").wireItem(t, f.ca)})))
	c, _ := f.client()
	st, err := c.GetAllSubscriptionStatuses(ctx, origTx)
	require.NoError(t, err)
	evs, err := EventsFromStatuses(st)
	require.NoError(t, err)
	require.Len(t, evs, 1)

	_, err = svc.Process(ctx, evs[0])
	require.NoError(t, err)
	_, err = svc.Process(ctx, evs[0])
	assert.ErrorIs(t, err, entitlements.ErrDuplicate)

	// A retry that re-fetches gets a re-signed response with the same
	// signedDate in this fixture, hence the same ID.
	st2, err := c.GetAllSubscriptionStatuses(ctx, origTx)
	require.NoError(t, err)
	evs2, _ := EventsFromStatuses(st2)
	assert.Equal(t, evs[0].ID, evs2[0].ID)
}

func TestReconcile_DoesNotOverwriteANewerWebhook(t *testing.T) {
	svc, _ := newService()
	// A webhook Apple signed after the snapshot was taken (it was in flight
	// while Reconcile ran) is already applied.
	_, err := svc.Process(ctx, entitlements.Event{
		ID: "uuid-expired", Source: entitlements.SourceApple, OriginalTxID: origTx, UserID: "u1",
		ProductID: "com.example.app.monthly", State: entitlements.StateExpired,
		ExpiresAt: time.UnixMilli(1790812800000).UTC(), OccurredAt: snapshotAt.Add(time.Second),
		Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)

	res, err := reconcileFixtures(t, svc, "status_active") // snapshot is older
	require.NoError(t, err)
	assert.Equal(t, OutcomeStale, res[0].Outcome)

	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ent.State, "the newer webhook wins")
	assert.EqualValues(t, 1, ent.Version)
}

func TestReconcile_OlderWebhookCannotUndoTheRepair(t *testing.T) {
	svc, _ := newService()
	_, err := reconcileFixtures(t, svc, "status_expired")
	require.NoError(t, err)

	// A DID_RENEW that Apple signed before the snapshot is delivered late.
	_, err = svc.Process(ctx, entitlements.Event{
		ID: "uuid-late-renew", Source: entitlements.SourceApple, OriginalTxID: origTx,
		ProductID: "com.example.app.monthly", State: entitlements.StateActive,
		ExpiresAt: time.UnixMilli(1794268800000).UTC(), OccurredAt: snapshotAt.Add(-time.Hour),
		Environment: entitlements.EnvironmentSandbox,
	})
	assert.ErrorIs(t, err, entitlements.ErrStale)
	ent, err := svc.Get(ctx, entitlements.SourceApple, origTx)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ent.State)
}

func TestReconcile_NewerWebhookAppliesOnTopOfTheSnapshot(t *testing.T) {
	svc, _ := newService()
	_, err := reconcileFixtures(t, svc, "status_expired")
	require.NoError(t, err)
	_, err = svc.Process(ctx, entitlements.Event{
		ID: "uuid-renew", Source: entitlements.SourceApple, OriginalTxID: origTx,
		ProductID: "com.example.app.monthly", State: entitlements.StateActive,
		ExpiresAt: time.UnixMilli(1794268800000).UTC(), OccurredAt: snapshotAt.Add(time.Minute),
		Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)
	ent, _ := svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.Equal(t, entitlements.StateActive, ent.State)
}

func TestReconcile_LiftsARefundAppleHasReversed(t *testing.T) {
	svc, _ := newService()
	_, err := svc.Process(ctx, entitlements.Event{
		ID: "uuid-refund", Source: entitlements.SourceApple, OriginalTxID: origTx, TransactionID: "2000000000000002",
		State: entitlements.StateRefunded, ExpiresAt: time.UnixMilli(1794268800000).UTC(),
		OccurredAt: snapshotAt.Add(-48 * time.Hour), Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)

	// REFUND_REVERSED was lost; Apple now reports the subscription active.
	res, err := reconcileFixtures(t, svc, "status_active")
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, res[0].Outcome)
	ent, _ := svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.Equal(t, entitlements.StateActive, ent.State)
	assert.Empty(t, ent.RefundedTransactions)
}

func TestReconcile_BillingRetrySnapshotLiftsARefund(t *testing.T) {
	svc, _ := newService()
	_, err := svc.Process(ctx, entitlements.Event{
		ID: "uuid-refund", Source: entitlements.SourceApple, OriginalTxID: origTx, TransactionID: "2000000000000002",
		State: entitlements.StateRefunded, OccurredAt: snapshotAt.Add(-48 * time.Hour), Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)
	res, err := reconcileFixtures(t, svc, "status_billing_retry")
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, res[0].Outcome, "Restores lets any live state lift a refund Apple no longer reports")
}

func TestReconcile_RecordsARefund(t *testing.T) {
	svc, _ := newService()
	_, err := reconcileFixtures(t, svc, "status_active")
	require.NoError(t, err)
	// Apple later reports the same chain refunded; the snapshot is newer.
	f := newFakeApple(t)
	later := loadStatus(t, "status_refunded").with(map[string]any{"signedDate": 1791633600000 + 3600_000})
	f.respond(ok(statusResponse("Sandbox", []map[string]any{later.wireItem(t, f.ca)})))
	c, _ := f.client()
	res, err := Reconcile(ctx, c, svc, origTx)
	require.NoError(t, err)
	assert.Equal(t, OutcomeApplied, res[0].Outcome)
	ent, _ := svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.Equal(t, entitlements.StateRefunded, ent.State)
	assert.Equal(t, []string{"2000000000000002"}, ent.RefundedTransactions)
}

func TestReconcile_SandboxIsIgnoredByAProductionService(t *testing.T) {
	svc := entitlements.NewService(memory.New()) // no WithAllowSandbox
	res, err := reconcileFixtures(t, svc, "status_active")
	require.NoError(t, err, "an ignored event is not a failure")
	assert.Equal(t, OutcomeIgnored, res[0].Outcome)
	_, err = svc.Get(ctx, entitlements.SourceApple, origTx)
	assert.ErrorIs(t, err, entitlements.ErrNotFound)
}

func TestReconcile_OneBadChainDoesNotBlockTheOthers(t *testing.T) {
	svc, _ := newService()
	f := newFakeApple(t)
	good := loadStatus(t, "status_active").with(map[string]any{"originalTransactionId": "chain-good", "transactionId": "g1"})
	bad := loadStatus(t, "status_active_no_expiry").with(map[string]any{"originalTransactionId": "chain-bad", "transactionId": "b1"})
	f.respond(ok(statusResponse("Sandbox", []map[string]any{bad.wireItem(t, f.ca), good.wireItem(t, f.ca)})))
	c, _ := f.client()

	res, err := Reconcile(ctx, c, svc, origTx)
	require.Error(t, err)
	assert.ErrorIs(t, err, apple.ErrNoExpiry)
	assert.ErrorContains(t, err, "chain chain-bad")
	require.Len(t, res, 2)
	assert.Equal(t, OutcomeFailed, res[0].Outcome)
	assert.Equal(t, OutcomeApplied, res[1].Outcome)
	_, gerr := svc.Get(ctx, entitlements.SourceApple, "chain-good")
	assert.NoError(t, gerr)
	_, berr := svc.Get(ctx, entitlements.SourceApple, "chain-bad")
	assert.ErrorIs(t, berr, entitlements.ErrNotFound, "no open-ended access from a date-less snapshot")
}

func TestReconcile_ReportsAppleErrors(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(http.StatusNotFound, 4040010, "Transaction id not found."))
	c, _ := f.client()
	svc, _ := newService()
	res, err := Reconcile(ctx, c, svc, "unknown")
	assert.Nil(t, res)
	assert.ErrorIs(t, err, ErrTransactionNotFound)
	assert.ErrorContains(t, err, "reconcile unknown")
}

type stubApplier struct {
	getErr, processErr error
	processed          int
}

func (s *stubApplier) Process(context.Context, entitlements.Event) (*entitlements.Entitlement, error) {
	s.processed++
	return nil, s.processErr
}

func (s *stubApplier) Get(context.Context, entitlements.Source, string) (*entitlements.Entitlement, error) {
	return nil, s.getErr
}

func TestReconcile_StoreFailuresAreFailures(t *testing.T) {
	boom := errors.New("db down")
	t.Run("Get fails", func(t *testing.T) {
		app := &stubApplier{getErr: boom}
		res, err := reconcileFixtures(t, app, "status_active")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, OutcomeFailed, res[0].Outcome)
		assert.Zero(t, app.processed, "do not write when the read failed")
	})
	t.Run("Process fails", func(t *testing.T) {
		app := &stubApplier{getErr: entitlements.ErrNotFound, processErr: boom}
		res, err := reconcileFixtures(t, app, "status_active")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, OutcomeFailed, res[0].Outcome)
		assert.ErrorIs(t, res[0].Err, boom)
	})
}

func TestClassify(t *testing.T) {
	wrap := func(e error) error { return errors.Join(errors.New("x"), e) }
	assert.Equal(t, OutcomeApplied, classify(nil))
	assert.Equal(t, OutcomeDuplicate, classify(wrap(entitlements.ErrDuplicate)))
	assert.Equal(t, OutcomeStale, classify(wrap(entitlements.ErrStale)))
	assert.Equal(t, OutcomeIgnored, classify(wrap(entitlements.ErrIgnored)))
	assert.Equal(t, OutcomeIgnored, classify(wrap(entitlements.ErrUnsupported)))
	assert.Equal(t, OutcomeFailed, classify(errors.New("db")))
	assert.Equal(t, OutcomeFailed, classify(wrap(entitlements.ErrInvalidEvent)))
}

func TestSameState(t *testing.T) {
	exp := time.UnixMilli(1794268800000).UTC()
	cur := &entitlements.Entitlement{State: entitlements.StateActive, ExpiresAt: exp, ProductID: "p", Environment: entitlements.EnvironmentSandbox}
	ev := entitlements.Event{State: entitlements.StateActive, ExpiresAt: exp, ProductID: "p", Environment: entitlements.EnvironmentSandbox}
	assert.True(t, sameState(cur, ev))
	for name, mod := range map[string]func(*entitlements.Event){
		"state":       func(e *entitlements.Event) { e.State = entitlements.StateGrace },
		"expiry":      func(e *entitlements.Event) { e.ExpiresAt = exp.Add(time.Second) },
		"product":     func(e *entitlements.Event) { e.ProductID = "q" },
		"environment": func(e *entitlements.Event) { e.Environment = entitlements.EnvironmentProduction },
	} {
		e := ev
		mod(&e)
		assert.False(t, sameState(cur, e), name)
	}
}

func TestEventsFromStatuses_ReportsSkippedChains(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(statusResponse("Sandbox",
		[]map[string]any{loadStatus(t, "status_active_no_expiry").wireItem(t, f.ca)},
		[]map[string]any{loadStatus(t, "status_expired").with(map[string]any{"originalTransactionId": "other"}).wireItem(t, f.ca)},
	)))
	c, _ := f.client()
	st, err := c.GetAllSubscriptionStatuses(ctx, origTx)
	require.NoError(t, err)
	evs, err := EventsFromStatuses(st)
	require.ErrorIs(t, err, apple.ErrNoExpiry)
	require.Len(t, evs, 1)
	assert.Equal(t, entitlements.StateExpired, evs[0].State)
}
