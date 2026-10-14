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
	"github.com/tggo/go-entitlements/google/mocks"
	"github.com/tggo/go-entitlements/internal/testutil"
)

const priorGoogle = "google-issue-derived/"

func priorSub(t *testing.T, name string) *google.SubscriptionPurchaseV2 {
	t.Helper()
	var s google.SubscriptionPurchaseV2
	require.NoError(t, jsonUnmarshal(testutil.PriorArtFixture(t, priorGoogle+name), &s))
	return &s
}

func parseWith(t *testing.T, sub *google.SubscriptionPurchaseV2, opts ...google.Option) (entitlements.Event, error) {
	t.Helper()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(sub, nil)
	body := testutil.GooglePushBody(t, "m1", testutil.GoogleFixture(t, "notif_subscription.json"))
	return newParser(t, f, opts...).Parse(context.Background(), body)
}

// Gap 2.
func TestGap2_NoExpiryAnywhereDoesNotGrantForever(t *testing.T) {
	t.Parallel()
	_, err := parseWith(t, priorSub(t, "sub_active_no_expiry_anywhere.json"))
	require.ErrorIs(t, err, google.ErrNoExpiry)
	assert.ErrorIs(t, err, entitlements.ErrUnsupported)
}

func TestGap2_DeferredReplacementKeepsCurrentItem(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_deferred_replacement_two_line_items.json"))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2025, 11, 1, 10, 0, 0, 0, time.UTC), res.ExpiresAt)
	assert.Equal(t, "pro.monthly", res.ProductID)
}

func TestGap2_NonGrantingStateWithoutExpiryIsFine(t *testing.T) {
	t.Parallel()
	sub := &google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired, LineItems: []google.LineItem{{ProductID: "p"}}}
	res, err := parseWith(t, sub)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, res.State)
}

// Gap 4.
func TestGap4_Voided(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fixture string
		wantErr error
	}{
		"one-time product ignored": {"google-play-billing-rtdn/voided-one-time-DERIVED.push.json", entitlements.ErrIgnored},
		"subscription full refund": {"google-play-billing-rtdn/voided-subscription.push.json", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := testutil.PriorArtFixture(t, tc.fixture)
			ev, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), raw)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateRefunded, ev.State)
		})
	}
}

func TestGap4_PartialRefundIgnored(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"packageName":"com.example.app","eventTimeMillis":"1760436000000","voidedPurchaseNotification":{"purchaseToken":"tok-1","orderId":"o","productType":1,"refundType":2}}`)
	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), testutil.GooglePushBody(t, "m", payload))
	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestGap4_MissingTypesTreatedAsFullSubscriptionRefund(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"packageName":"com.example.app","eventTimeMillis":"1760436000000","voidedPurchaseNotification":{"purchaseToken":"tok-1"}}`)
	ev, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), testutil.GooglePushBody(t, "m", payload))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, ev.State)
}

// Gap 5.
func TestGap5_TestPurchaseMarked(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_active_obfuscated_account_only.json"))
	require.NoError(t, err)
	assert.True(t, res.Sandbox)
	assert.Equal(t, entitlements.StateActive, res.State)
}

func TestGap5_RealPurchaseNotMarked(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_active_profile_id_only.json"))
	require.NoError(t, err)
	assert.False(t, res.Sandbox)
}

func TestGap5_RejectTestPurchases(t *testing.T) {
	t.Parallel()
	_, err := parseWith(t, priorSub(t, "sub_active_obfuscated_account_only.json"), google.WithRejectTestPurchases())
	require.ErrorIs(t, err, google.ErrTestPurchase)
	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestGap5_ParseStillWorksForTestPurchaseByDefault(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(priorSub(t, "sub_active_obfuscated_account_only.json"), nil)
	body := testutil.GooglePushBody(t, "m1", testutil.GoogleFixture(t, "notif_subscription.json"))
	ev, err := newParser(t, f).Parse(context.Background(), body)
	require.NoError(t, err)
	assert.Equal(t, "VioUT4kHwh0ur8crMexs", ev.UserID)
	assert.True(t, ev.Sandbox)
}

// Gap 7.
func TestGap7_ProfileIDFallback(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_active_profile_id_only.json"))
	require.NoError(t, err)
	assert.Equal(t, "profile-1234", res.UserID)
}

func TestGap7_AccountIDWinsOverProfileID(t *testing.T) {
	t.Parallel()
	sub := priorSub(t, "sub_active_profile_id_only.json")
	sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountID = "acct-1"
	res, err := parseWith(t, sub)
	require.NoError(t, err)
	assert.Equal(t, "acct-1", res.UserID)
}

// Gap 9.
func graceSub(expiry time.Time) *google.SubscriptionPurchaseV2 {
	return &google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateInGracePeriod,
		LineItems:         []google.LineItem{{ProductID: "p", ExpiryTime: expiry}},
	}
}

func TestGap9_GraceWithPastExpiryIsActiveUntilGraceEnd(t *testing.T) {
	t.Parallel()
	occurred := time.UnixMilli(1760436000000).UTC()
	res, err := parseWith(t, graceSub(occurred.Add(-time.Hour))) // original expiry already passed
	require.NoError(t, err)
	assert.Equal(t, occurred.Add(google.DefaultGraceDuration), res.ExpiresAt)
	ent := entitlements.Entitlement{State: res.State, ExpiresAt: res.ExpiresAt}
	assert.True(t, ent.Active(occurred.Add(24*time.Hour)))
	assert.False(t, ent.Active(occurred.Add(google.DefaultGraceDuration+time.Second)))
}

func TestGap9_GraceDurationOption(t *testing.T) {
	t.Parallel()
	occurred := time.UnixMilli(1760436000000).UTC()
	res, err := parseWith(t, graceSub(occurred.Add(-time.Hour)), google.WithGraceDuration(3*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, occurred.Add(72*time.Hour), res.ExpiresAt)
}

func TestGap9_GraceWithFutureExpiryKept(t *testing.T) {
	t.Parallel()
	occurred := time.UnixMilli(1760436000000).UTC()
	far := occurred.Add(60 * 24 * time.Hour)
	res, err := parseWith(t, graceSub(far))
	require.NoError(t, err)
	assert.Equal(t, far, res.ExpiresAt)
}

func TestGap9_GraceWithoutExpiryUsesGraceWindow(t *testing.T) {
	t.Parallel()
	occurred := time.UnixMilli(1760436000000).UTC()
	res, err := parseWith(t, graceSub(time.Time{}))
	require.NoError(t, err)
	assert.Equal(t, occurred.Add(google.DefaultGraceDuration), res.ExpiresAt)
}
