package google_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
)

// ParseToken syncs state from a purchase token the app sends right after a
// purchase, without waiting for a push.
func TestParseToken_MapsLikeAPush(t *testing.T) {
	t.Parallel()
	s := sub(google.StateActive, item("premium", tBase))
	s.AcknowledgementState = ackPending
	f := fetcherFor(t, fetchMap{"tok": s}, nil)

	res, err := newParser(t, f, atClock()).ParseToken(context.Background(), "premium", "tok")

	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, entitlements.SourceGoogle, ev.Source)
	assert.Equal(t, "tok", ev.OriginalTxID)
	assert.Equal(t, "premium", ev.ProductID)
	assert.Equal(t, entitlements.StateActive, ev.State)
	assert.Equal(t, tBase, ev.ExpiresAt)
	assert.Equal(t, "user-1", ev.UserID)
	require.NotNil(t, res.Ack)
	assert.Equal(t, google.Ack{PackageName: pkg, SubscriptionID: "premium", PurchaseToken: "tok"}, *res.Ack)
}

func TestParseToken_RepeatsDedupeAndChangesDoNot(t *testing.T) {
	t.Parallel()
	parse := func(s *google.SubscriptionPurchaseV2, now time.Time) []entitlements.Event {
		f := fetcherFor(t, fetchMap{"tok": s}, nil)
		p := newParser(t, f, google.WithTimeFn(func() time.Time { return now }))
		res, err := p.ParseToken(context.Background(), "premium", "tok")
		require.NoError(t, err)
		return res.Events
	}
	first := parse(sub(google.StateActive, item("premium", tBase)), clockNow)
	again := parse(sub(google.StateActive, item("premium", tBase)), clockNow.Add(3*time.Hour))
	renewed := parse(sub(google.StateActive, item("premium", tBase.Add(30*24*time.Hour))), clockNow)
	canceled := parse(sub(google.StateExpired, item("premium", tBase)), clockNow)

	assert.Equal(t, first[0].ID, again[0].ID, "the same state fetched later is the same event")
	assert.NotEqual(t, first[0].ID, renewed[0].ID)
	assert.NotEqual(t, first[0].ID, canceled[0].ID)
	assert.LessOrEqual(t, len(first[0].ID), 256, "fits the core identifier limit")
	assert.NotContains(t, first[0].ID, "tok", "the purchase token is a secret and stays out of ids")
}

// The ordering key is the fetch time minus the clock-skew margin, so a sync
// can never overwrite a push that is newer than the state it fetched.
func TestParseToken_CannotOverwriteANewerPush(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateExpired, item("premium", tPast))}, nil)
	res, err := newParser(t, f, atClock()).ParseToken(context.Background(), "premium", "tok")
	require.NoError(t, err)
	sync := res.Events[0]
	assert.Equal(t, clockNow.Add(-google.DefaultMaxFutureSkew), sync.OccurredAt)

	// A push applied one minute before the sync started is inside the margin:
	// the sync is stale.
	push := entitlements.Event{ID: "p", Source: entitlements.SourceGoogle, OriginalTxID: "tok", ProductID: "premium",
		State: entitlements.StateActive, ExpiresAt: tBase, OccurredAt: clockNow.Add(-time.Minute)}
	cur, err := entitlements.Apply(nil, push)
	require.NoError(t, err)
	_, err = entitlements.Apply(cur, sync)
	require.ErrorIs(t, err, entitlements.ErrStale)

	// A push from before the margin is older than the fetched state: the sync wins.
	old := push
	old.OccurredAt = clockNow.Add(-time.Hour)
	cur, err = entitlements.Apply(nil, old)
	require.NoError(t, err)
	next, err := entitlements.Apply(cur, sync)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, next.State)

	// Pushes after the sync still apply.
	later := push
	later.OccurredAt = clockNow.Add(time.Minute)
	_, err = entitlements.Apply(next, later)
	assert.NoError(t, err)
}

func TestParseToken_RejectsBadInput(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{}, nil)
	p := newParser(t, f, atClock())
	for name, tc := range map[string][2]string{
		"empty token":      {"premium", ""},
		"control in token": {"premium", "a\x00b"},
		"control in sku":   {"pre\nmium", "tok"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseToken(context.Background(), tc[0], tc[1])
			assert.ErrorIs(t, err, google.ErrMalformed)
		})
	}
}

func TestParseToken_NoSubscriptionIDIsFine(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("premium", tBase))}, nil)

	res, err := newParser(t, f, atClock()).ParseToken(context.Background(), "", "tok")

	require.NoError(t, err)
	assert.Equal(t, "premium", res.Events[0].ProductID)
}

func TestParseToken_RespectsTestPurchaseRejection(t *testing.T) {
	t.Parallel()
	s := sub(google.StateActive, item("premium", tBase))
	s.TestPurchase = true
	f := fetcherFor(t, fetchMap{"tok": s}, nil)

	_, err := newParser(t, f, atClock(), google.WithRejectTestPurchases()).ParseToken(context.Background(), "premium", "tok")

	assert.ErrorIs(t, err, google.ErrTestPurchase)
}
