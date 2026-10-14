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
	return newParser(t, f, append([]google.Option{atClock()}, opts...)...).Parse(context.Background(), body)
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
			f := mocks.NewMockSubscriptionsFetcher(t) // one-time: never fetched
			if tc.wantErr == nil {
				f.EXPECT().GetV2(mock.Anything, "com.some.app", "PURCHASE_TOKEN").
					Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil)
			}
			ev, err := newParserFor(t, f, "com.some.app").Parse(context.Background(), raw)
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
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil)
	ev, err := newParser(t, f).Parse(context.Background(), testutil.GooglePushBody(t, "m", payload))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, ev.State)
}

// Gap 5.
func TestGap5_TestPurchaseMarked(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_active_obfuscated_account_only.json"))
	require.NoError(t, err)
	assert.Equal(t, entitlements.EnvironmentGoogleTest, res.Environment)
	assert.True(t, res.Sandbox())
	assert.Equal(t, entitlements.StateActive, res.State)
}

func TestGap5_RealPurchaseNotMarked(t *testing.T) {
	t.Parallel()
	res, err := parseWith(t, priorSub(t, "sub_active_profile_id_only.json"))
	require.NoError(t, err)
	assert.False(t, res.Sandbox())
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
	assert.True(t, ev.Sandbox())
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

// Gap 9 (grace) is covered by TestGrace_TrustsExpiryTime in security_test.go.
func graceSub(expiry time.Time) *google.SubscriptionPurchaseV2 {
	return &google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateInGracePeriod,
		LineItems:         []google.LineItem{{ProductID: "p", ExpiryTime: expiry}},
	}
}
