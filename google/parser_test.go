package google_test

import (
	"context"
	"errors"
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

const pkg = "com.example.app"

func fetchedSub(t *testing.T, name string) *google.SubscriptionPurchaseV2 {
	t.Helper()
	var s google.SubscriptionPurchaseV2
	require.NoError(t, jsonUnmarshal(testutil.GoogleFixture(t, name), &s))
	return &s
}

func TestParser_SubscriptionStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture string
		want    entitlements.State
	}{
		{"sub_state_active.json", entitlements.StateActive},
		{"sub_state_canceled.json", entitlements.StateActive},
		{"sub_state_in_grace_period.json", entitlements.StateGrace},
		{"sub_state_on_hold.json", entitlements.StateBillingRetry},
		{"sub_state_paused.json", entitlements.StatePaused},
		{"sub_state_expired.json", entitlements.StateExpired},
		{"sub_state_pending_purchase_canceled.json", entitlements.StateExpired},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(fetchedSub(t, tc.fixture), nil)
			body := testutil.GooglePushBody(t, "msg-1", testutil.GoogleFixture(t, "notif_subscription.json"))

			ev, err := newParser(t, f).Parse(context.Background(), body)

			require.NoError(t, err)
			assert.Equal(t, tc.want, ev.State)
			assert.Equal(t, "msg-1", ev.ID)
			assert.Equal(t, entitlements.SourceGoogle, ev.Source)
			assert.Equal(t, "user-1", ev.UserID)
			assert.Equal(t, "tok-1", ev.OriginalTxID)
			assert.Equal(t, "premium.monthly", ev.ProductID)
			assert.Equal(t, time.UnixMilli(1760436000000).UTC(), ev.OccurredAt)
			assert.Equal(t, time.Date(2026, 11, 14, 10, 0, 0, 123_000_000, time.UTC), ev.ExpiresAt)
			assert.Equal(t, body, ev.Raw)
		})
	}
}

func TestParser_PendingIsIgnored(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(fetchedSub(t, "sub_state_pending.json"), nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	_, err := newParser(t, f).Parse(context.Background(), body)

	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestParser_UnknownStateUnsupported(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(fetchedSub(t, "sub_state_unknown.json"), nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	_, err := newParser(t, f).Parse(context.Background(), body)

	assert.ErrorIs(t, err, entitlements.ErrUnsupported)
}

func TestParser_LinkedTokenUpgradePicksLatestExpiry(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "new-token").Return(fetchedSub(t, "sub_linked_upgrade.json"), nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription_upgrade.json"))

	ev, err := newParser(t, f).Parse(context.Background(), body)

	require.NoError(t, err)
	assert.Equal(t, "new-token", ev.OriginalTxID)
	assert.Equal(t, "old-token", ev.SupersedesTxID)
	assert.Equal(t, "premium.yearly", ev.ProductID)
	assert.Equal(t, time.Date(2027, 10, 14, 10, 0, 0, 0, time.UTC), ev.ExpiresAt)
}

func TestParser_NoLineItemsRefusedWhenGranting(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").
		Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateActive}, nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	_, err := newParser(t, f).Parse(context.Background(), body)

	assert.ErrorIs(t, err, google.ErrNoExpiry)
}

func TestParser_NoLineItemsKeepsNotificationProductWhenNotGranting(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").
		Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	ev, err := newParser(t, f).Parse(context.Background(), body)

	require.NoError(t, err)
	assert.Equal(t, "premium.monthly", ev.ProductID)
	assert.True(t, ev.ExpiresAt.IsZero())
	assert.Empty(t, ev.UserID)
}

func TestParser_VoidedIsVerifiedByFetch(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil).Once()
	body := testutil.GooglePushBody(t, "msg-v", testutil.GoogleFixture(t, "notif_voided.json"))

	ev, err := newParser(t, f).Parse(context.Background(), body)

	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, ev.State)
	assert.Equal(t, "tok-1", ev.OriginalTxID)
	assert.Equal(t, "msg-v", ev.ID)
}

func TestParser_TestNotificationIgnored(t *testing.T) {
	t.Parallel()
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_test.json"))

	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), body)

	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestParser_UnknownNotificationUnsupported(t *testing.T) {
	t.Parallel()
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_unknown.json"))

	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), body)

	assert.ErrorIs(t, err, entitlements.ErrUnsupported)
}

func TestParser_WrongPackage(t *testing.T) {
	t.Parallel()
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_wrong_package.json"))

	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), body)

	assert.ErrorIs(t, err, google.ErrPackageMismatch)
}

func TestParser_FetcherError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(nil, boom)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	_, err := newParser(t, f).Parse(context.Background(), body)

	assert.ErrorIs(t, err, boom)
}

func TestParser_FetcherReturnsNil(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(nil, nil)
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_subscription.json"))

	_, err := newParser(t, f).Parse(context.Background(), body)

	require.ErrorIs(t, err, google.ErrNoSubscription)
}

func TestParser_Malformed(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"not json envelope": []byte("{"),
		"bad base64":        testutil.GoogleFixture(t, "envelope_bad_base64.json"),
		"no message id":     testutil.GoogleFixture(t, "envelope_no_message_id.json"),
		"payload not json":  testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_bad_json.json")),
		"empty token":       testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_empty_token.json")),
		"bad millis":        testutil.GooglePushBody(t, "m", []byte(`{"packageName":"x","eventTimeMillis":"abc"}`)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), body)
			assert.ErrorIs(t, err, google.ErrMalformed)
		})
	}
}

func TestMillis(t *testing.T) {
	t.Parallel()
	var d google.DeveloperNotification
	require.NoError(t, jsonUnmarshal([]byte(`{"eventTimeMillis":1760436000000}`), &d))
	assert.Equal(t, time.UnixMilli(1760436000000).UTC(), d.EventTimeMillis.Time())

	require.NoError(t, jsonUnmarshal([]byte(`{"eventTimeMillis":null}`), &d))
	assert.True(t, d.EventTimeMillis.Time().IsZero())
	require.NoError(t, jsonUnmarshal([]byte(`{"eventTimeMillis":""}`), &d))
	assert.True(t, d.EventTimeMillis.Time().IsZero())
	require.ErrorContains(t, jsonUnmarshal([]byte(`{"eventTimeMillis":"1.5x"}`), &d), `invalid millis "1.5x"`)
}
