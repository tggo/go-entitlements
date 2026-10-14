package google_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/google"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func activeItems(items ...google.LineItem) *google.SubscriptionPurchaseV2 {
	return &google.SubscriptionPurchaseV2{SubscriptionState: google.StateActive, LineItems: items}
}

func TestRejectTestPurchases_RealPurchaseStillAccepted(t *testing.T) {
	t.Parallel()
	ev, err := parseWith(t, priorSub(t, "sub_active_profile_id_only.json"), google.WithRejectTestPurchases())
	require.NoError(t, err)
	assert.False(t, ev.Sandbox())
	assert.Equal(t, entitlements.StateActive, ev.State)
}

func TestLineItemSelection(t *testing.T) {
	t.Parallel()
	soon, later := clockNow.Add(24*time.Hour), clockNow.Add(48*time.Hour)
	cases := map[string]struct {
		sub         *google.SubscriptionPurchaseV2
		wantProduct string
		wantExpiry  time.Time
	}{
		"zero-expiry item first never wins": {
			sub:         activeItems(google.LineItem{ProductID: "replacement"}, google.LineItem{ProductID: "current", ExpiryTime: soon}),
			wantProduct: "current", wantExpiry: soon,
		},
		"latest expiry wins regardless of order, later first": {
			sub:         activeItems(google.LineItem{ProductID: "b", ExpiryTime: later}, google.LineItem{ProductID: "a", ExpiryTime: soon}),
			wantProduct: "b", wantExpiry: later,
		},
		"latest expiry wins regardless of order, later last": {
			sub:         activeItems(google.LineItem{ProductID: "a", ExpiryTime: soon}, google.LineItem{ProductID: "b", ExpiryTime: later}),
			wantProduct: "b", wantExpiry: later,
		},
		"equal expiries keep the first": {
			sub:         activeItems(google.LineItem{ProductID: "a", ExpiryTime: soon}, google.LineItem{ProductID: "b", ExpiryTime: soon}),
			wantProduct: "a", wantExpiry: soon,
		},
		"winner without product keeps the notification subscriptionId": {
			sub: activeItems(google.LineItem{ExpiryTime: soon}), wantProduct: "premium.monthly", wantExpiry: soon,
		},
		"no expiry anywhere, non-granting: first item's product": {
			sub: &google.SubscriptionPurchaseV2{
				SubscriptionState: google.StateExpired,
				LineItems:         []google.LineItem{{ProductID: "first"}, {ProductID: "second"}},
			},
			wantProduct: "first",
		},
		"no expiry anywhere, first item without product": {
			sub: &google.SubscriptionPurchaseV2{
				SubscriptionState: google.StateExpired,
				LineItems:         []google.LineItem{{}, {ProductID: "second"}},
			},
			wantProduct: "premium.monthly",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev, err := parseWith(t, tc.sub)
			require.NoError(t, err)
			assert.Equal(t, tc.wantProduct, ev.ProductID)
			assert.Equal(t, tc.wantExpiry, ev.ExpiresAt)
		})
	}
}

func TestOptions_NonPositiveValuesKeepDefaults(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Second} {
		t.Run("fetch timeout "+d.String(), func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").
				RunAndReturn(func(ctx context.Context, _, _ string) (*google.SubscriptionPurchaseV2, error) {
					dl, ok := ctx.Deadline()
					require.True(t, ok)
					assert.WithinDuration(t, time.Now().Add(google.DefaultFetchTimeout), dl, 2*time.Second)
					return activeItems(google.LineItem{ProductID: "p", ExpiryTime: clockNow.Add(time.Hour)}), nil
				})
			_, err := newParser(t, f, atClock(), google.WithFetchTimeout(d)).Parse(context.Background(), subBody(t))
			require.NoError(t, err)
		})
		t.Run("future skew "+d.String(), func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "T").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil).Maybe()
			p := newParser(t, f, atClock(), google.WithMaxFutureSkew(d))

			_, err := p.Parse(context.Background(), voidedBody(t, "T", "o", clockNow.Add(google.DefaultMaxFutureSkew-time.Minute).UnixMilli()))
			assert.NotErrorIs(t, err, google.ErrMalformed, "inside the default skew")
			_, err = p.Parse(context.Background(), voidedBody(t, "T", "o", clockNow.Add(google.DefaultMaxFutureSkew+time.Minute).UnixMilli()))
			assert.ErrorIs(t, err, google.ErrMalformed, "outside the default skew")
		})
	}
	var m google.Millis
	require.NoError(t, m.UnmarshalJSON(nil), "empty input is zero, not a panic")
	assert.Zero(t, m)
}

func TestVoided_MixedOrderIDsAcrossItems(t *testing.T) {
	t.Parallel()
	sub := &google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateActive,
		LineItems: []google.LineItem{
			{ProductID: "a", ExpiryTime: clockNow.Add(time.Hour)},
			{ProductID: "b", ExpiryTime: clockNow.Add(time.Hour), LatestSuccessfulOrderID: "GPA.2"},
		},
	}
	for orderID, wantRefund := range map[string]bool{"GPA.2": true, "GPA.1": false, "": false} {
		t.Run("order "+orderID, func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "T").Return(sub, nil)

			ev, err := newParser(t, f, atClock()).Parse(context.Background(), voidedBody(t, "T", orderID, clockNow.UnixMilli()))

			if wantRefund {
				require.NoError(t, err)
				assert.Equal(t, entitlements.StateRefunded, ev.State)
			} else {
				assert.ErrorIs(t, err, entitlements.ErrIgnored)
			}
		})
	}
}

func TestDispatch_EachNotificationKindIsHandled(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, mock.Anything).Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil).Maybe()
	p := newParser(t, f, atClock())
	for fixture, want := range map[string]error{
		"notif_test.json":    google.ErrTestNotification,
		"notif_unknown.json": entitlements.ErrUnsupported,
	} {
		_, err := p.Parse(context.Background(), testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, fixture)))
		require.ErrorIs(t, err, want, fixture)
	}
	ev, err := p.Parse(context.Background(), voidedBody(t, "T", "o", clockNow.UnixMilli()))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, ev.State)
	ev, err = p.Parse(context.Background(), subBody(t))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ev.State)
}
