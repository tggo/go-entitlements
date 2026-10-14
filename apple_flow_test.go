package entitlements_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/memory"
)

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := append(append(append([]int{}, p[:i]...), n-1), p[i:]...)
			out = append(out, q)
		}
	}
	return out
}

// runChain feeds three userless events (oldest first: subscribed, renewal
// status change, expired) through parser+Service+memory store in every
// delivery order and checks the end state is expired.
func newSandboxSvc(t *testing.T, st entitlements.Store) *entitlements.Service {
	t.Helper()
	return entitlements.NewService(st, entitlements.WithLogger(testutil.NewLogger(t)), entitlements.WithAllowSandbox())
}

func runChain(t *testing.T, parser *apple.Parser, bodies [][]byte) {
	t.Helper()
	for _, order := range permutations(len(bodies)) {
		st := memory.New()
		svc := newSandboxSvc(t, st)
		var maxIdx = -1
		applied := 0
		for _, i := range order {
			ev, err := parser.Parse(bodies[i])
			require.NoError(t, err)
			assert.Empty(t, ev.UserID)
			_, err = svc.Process(context.Background(), ev)
			if i > maxIdx {
				maxIdx = i
				applied++
				require.NoError(t, err, "order %v", order)
			} else {
				assert.ErrorIs(t, err, entitlements.ErrStale, "order %v", order)
			}
		}
		ev, _ := parser.Parse(bodies[0])
		got, err := st.Get(context.Background(), entitlements.SourceApple, ev.OriginalTxID)
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateExpired, got.State, "order %v", order)
		assert.Equal(t, int64(applied), got.Version, "order %v", order)
		assert.Empty(t, got.UserID)
	}
	// Delivered order: all three applied.
	st := memory.New()
	svc := newSandboxSvc(t, st)
	for _, b := range bodies {
		ev, err := parser.Parse(b)
		require.NoError(t, err)
		_, err = svc.Process(context.Background(), ev)
		require.NoError(t, err)
	}
	ev, _ := parser.Parse(bodies[0])
	got, err := st.Get(context.Background(), entitlements.SourceApple, ev.OriginalTxID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.Version)
}

func TestAppleChain_Synthetic_UserlessAllOrders(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	bodies := make([][]byte, 0, 3)
	for i, name := range []string{"subscribed", "renewal_status_off", "expired"} {
		f := testutil.LoadAppleFixture(t, name)
		delete(f.Transaction, "appAccountToken")
		f.Transaction["expiresDate"] = float64(1760500000000)
		f.Notification["signedDate"] = float64(1760000000000 + int64(i)*1000)
		if name == "expired" {
			f.Notification["signedDate"] = float64(1760500001000)
		}
		bodies = append(bodies, f.SignBody(t, ca))
	}
	runChain(t, apple.NewParser(mustVerifier(t, ca, apple.WithInsecureAnyBundle())), bodies)
}

func TestAppleChain_Real_UserlessAllOrders(t *testing.T) {
	dir := "testdata/real/apple"
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Skip("no real fixtures")
	}
	bodies := make([][]byte, len(files))
	for i, f := range files {
		var err error
		bodies[i], err = os.ReadFile(f)
		require.NoError(t, err)
	}
	signed := testutil.LatestSignedDate(t, bodies...) // see TestRealFixtures: the clock is pinned to the capture
	vf, err := apple.NewVerifier("menu.fridge.recipebox", apple.WithTimeFn(func() time.Time { return signed }))
	require.NoError(t, err)
	parser := apple.NewParser(vf)
	byType := map[string][]byte{}
	for _, b := range bodies {
		n, err := vf.VerifyNotification(b)
		require.NoError(t, err)
		byType[n.Payload.NotificationType] = b
	}
	runChain(t, parser, [][]byte{byType["SUBSCRIBED"], byType["DID_CHANGE_RENEWAL_STATUS"], byType["EXPIRED"]})
}

// appleBody signs fixture name with ca after applying mut to it.
func appleBody(t *testing.T, ca *testutil.AppleCA, name string, mut func(f *testutil.AppleFixture)) []byte {
	t.Helper()
	f := testutil.LoadAppleFixture(t, name)
	if mut != nil {
		mut(&f)
	}
	return f.SignBody(t, ca)
}

func at(ms int64) func(f *testutil.AppleFixture) {
	return func(f *testutil.AppleFixture) { f.Notification["signedDate"] = float64(ms) }
}

func process(t *testing.T, p *apple.Parser, svc *entitlements.Service, body []byte) (*entitlements.Entitlement, error) {
	t.Helper()
	ev, err := p.Parse(body)
	require.NoError(t, err)
	return svc.Process(context.Background(), ev)
}

const (
	msSub     = int64(1760000000000)
	msExpiry  = int64(1760500000000)
	msExpiry2 = int64(1763000000000)
)

func TestApple_RefundReversedRestoresChain(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	svc := newSvc(t, memory.New())

	_, err := process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	require.NoError(t, err)
	got, err := process(t, p, svc, appleBody(t, ca, "refund", at(msSub+1000)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, got.State)

	got, err = process(t, p, svc, appleBody(t, ca, "refund_reversed", at(msSub+2000)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.True(t, got.Active(time.UnixMilli(msSub+5000)))
	assert.Equal(t, time.UnixMilli(msExpiry).UTC(), got.ExpiresAt)

	// A reversal older than the refund must not restore.
	svc2 := newSvc(t, memory.New())
	_, _ = process(t, p, svc2, appleBody(t, ca, "subscribed", at(msSub)))
	_, _ = process(t, p, svc2, appleBody(t, ca, "refund", at(msSub+3000)))
	_, err = process(t, p, svc2, appleBody(t, ca, "refund_reversed", at(msSub+2000)))
	assert.ErrorIs(t, err, entitlements.ErrStale)
}

func TestApple_RefundReversedAfterExpiry(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	svc := newSvc(t, memory.New())
	_, _ = process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	_, _ = process(t, p, svc, appleBody(t, ca, "refund", at(msSub+1000)))
	// Reversed after the subscription period already ended: chain is expired, not active.
	got, err := process(t, p, svc, appleBody(t, ca, "refund_reversed", func(f *testutil.AppleFixture) {
		at(msExpiry + 1000)(f)
		f.Data()["status"] = float64(2)
	}))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, got.State)
}

func TestApple_RenewalExtendedMovesExpiry(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	svc := newSvc(t, memory.New())
	_, _ = process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	got, err := process(t, p, svc, appleBody(t, ca, "renewal_extended", at(msSub+1000)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Equal(t, time.UnixMilli(msExpiry2).UTC(), got.ExpiresAt)
	assert.True(t, got.Active(time.UnixMilli(msExpiry+1000)), "access continues past the old expiry")
}

func TestApple_RenewalExtensionSummaryIgnored(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	_, err := apple.NewParser(mustVerifier(t, ca)).Parse(appleBody(t, ca, "renewal_extension_summary", nil))
	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func renewalTx(id string, signed int64) func(f *testutil.AppleFixture) {
	return func(f *testutil.AppleFixture) {
		at(signed)(f)
		f.Transaction["transactionId"] = id
	}
}

func TestApple_RefundOfOlderTransactionKeepsChain(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	st := memory.New()
	svc := newSvc(t, st)
	_, _ = process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	_, err := process(t, p, svc, appleBody(t, ca, "did_renew_next_period", renewalTx("2000000000003", msSub+1000)))
	require.NoError(t, err)

	// Refund of the first transaction (…002, expiresDate msExpiry < current msExpiry2).
	_, err = process(t, p, svc, appleBody(t, ca, "refund", at(msSub+2000)))
	require.NoError(t, err, "recorded, not dropped")
	cur, gerr := st.Get(context.Background(), entitlements.SourceApple, "2000000000001")
	require.NoError(t, gerr)
	assert.Equal(t, entitlements.StateActive, cur.State)
	assert.Equal(t, time.UnixMilli(msExpiry2).UTC(), cur.ExpiresAt)
	assert.Equal(t, []string{"2000000000002"}, cur.RefundedTransactions)

	// Refund of the latest transaction still ends the chain.
	got, err := process(t, p, svc, appleBody(t, ca, "refund", func(f *testutil.AppleFixture) {
		renewalTx("2000000000003", msSub+3000)(f)
		f.Notification["notificationUUID"] = "uuid-refund-latest"
		f.Transaction["expiresDate"] = float64(msExpiry2)
	}))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, got.State)
	assert.Equal(t, []string{"2000000000002", "2000000000003"}, got.RefundedTransactions)
}

// The refund of the first transaction is delivered before the renewal that
// replaced it, and the renewal was signed before the refund.
func TestApple_RenewalDeliveredAfterTheRefundOfTheTransactionItReplaced(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	svc := newSvc(t, memory.New())
	_, err := process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	require.NoError(t, err)
	got, err := process(t, p, svc, appleBody(t, ca, "refund", at(msSub+2000)))
	require.NoError(t, err)
	require.Equal(t, entitlements.StateRefunded, got.State)

	got, err = process(t, p, svc, appleBody(t, ca, "did_renew_next_period", renewalTx("2000000000003", msSub+1000)))
	require.NoError(t, err, "the renewal is a different transaction")
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Equal(t, time.UnixMilli(msExpiry2).UTC(), got.ExpiresAt)
	ok, err := svc.IsActive(context.Background(), "7e3fb20b-4cdb-47cc-936d-99d65f608138", "com.example.pro.monthly")
	require.NoError(t, err)
	assert.False(t, ok, "IsActive uses the injected clock, which is well past the 2025 expiry")
}

func mustVerifier(t *testing.T, ca *testutil.AppleCA, opts ...apple.Option) *apple.Verifier {
	t.Helper()
	v, err := apple.NewVerifier("com.example.app", append([]apple.Option{apple.WithRootPool(ca.Pool)}, opts...)...)
	require.NoError(t, err)
	return v
}

func TestApple_UpgradeKeepsOneEntitlement(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	st := memory.New()
	svc := newSvc(t, st)
	_, _ = process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	got, err := process(t, p, svc, appleBody(t, ca, "did_change_renewal_pref", func(f *testutil.AppleFixture) {
		f.Notification["signedDate"] = float64(msSub + 1000)
		f.Transaction["productId"] = "com.example.pro.yearly"
	}))
	require.NoError(t, err)
	assert.Equal(t, "com.example.pro.yearly", got.ProductID)
	got, err = process(t, p, svc, appleBody(t, ca, "did_renew_next_period", func(f *testutil.AppleFixture) {
		f.Notification["signedDate"] = float64(msSub + 2000)
		f.Transaction["productId"] = "com.example.pro.yearly"
	}))
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.Version)
	assert.Equal(t, "com.example.pro.yearly", got.ProductID)
}

// Pinned behaviour: events with the same signedDate are both applied, the
// last delivered wins (Apply drops only strictly older events).
func TestApple_EqualSignedDateLastDeliveredWins(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	svc := newSvc(t, memory.New())
	_, _ = process(t, p, svc, appleBody(t, ca, "subscribed", at(msSub)))
	_, err := process(t, p, svc, appleBody(t, ca, "did_fail_to_renew", at(msSub+1000)))
	require.NoError(t, err)
	got, err := process(t, p, svc, appleBody(t, ca, "did_renew", at(msSub+1000)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Equal(t, int64(3), got.Version)
}

func TestApple_TokenAppearsLater(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := apple.NewParser(mustVerifier(t, ca))
	st := memory.New()
	svc := newSvc(t, st)
	userless := func(f *testutil.AppleFixture) { delete(f.Transaction, "appAccountToken") }
	_, err := process(t, p, svc, appleBody(t, ca, "subscribed", func(f *testutil.AppleFixture) { userless(f); at(msSub)(f) }))
	require.NoError(t, err)
	list, err := st.ListByUser(context.Background(), "7e3fb20b-4cdb-47cc-936d-99d65f608138")
	require.NoError(t, err)
	assert.Empty(t, list)

	got, err := process(t, p, svc, appleBody(t, ca, "did_renew", at(msSub+1000)))
	require.NoError(t, err)
	assert.Equal(t, "7e3fb20b-4cdb-47cc-936d-99d65f608138", got.UserID)
	list, err = st.ListByUser(context.Background(), got.UserID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestApply_SandboxFlagFollowsEvent(t *testing.T) {
	ev := entitlements.Event{ID: "a", Source: entitlements.SourceApple, OriginalTxID: "t", State: entitlements.StateActive, Environment: entitlements.EnvironmentSandbox, OccurredAt: time.UnixMilli(1)}
	e, err := entitlements.Apply(nil, ev)
	require.NoError(t, err)
	assert.True(t, e.Sandbox())
	ev.Environment, ev.OccurredAt = entitlements.EnvironmentProduction, time.UnixMilli(2)
	e, err = entitlements.Apply(e, ev)
	require.NoError(t, err)
	assert.False(t, e.Sandbox())
}
