package google_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

var clockNow = time.UnixMilli(1760436000000).UTC().Add(time.Hour)

func atClock() google.Option { return google.WithTimeFn(func() time.Time { return clockNow }) }

func voidedBody(tb testing.TB, token, orderID string, eventMillis int64) []byte {
	tb.Helper()
	payload := fmt.Sprintf(`{"packageName":%q,"eventTimeMillis":"%d","voidedPurchaseNotification":{"purchaseToken":%q,"orderId":%q,"productType":1,"refundType":1}}`,
		pkg, eventMillis, token, orderID)
	return testutil.GooglePushBody(tb, "msg-void", []byte(payload))
}

// Security H1: a forged voided push must not refund a chain Google does not confirm.
func TestVoided_OrderMismatchIsIgnored(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "VICTIM").Return(&google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateActive,
		LineItems:         []google.LineItem{{ProductID: "p", ExpiryTime: clockNow.Add(time.Hour), LatestSuccessfulOrderID: "GPA.2"}},
	}, nil)

	_, err := newParser(t, f, atClock()).Parse(context.Background(), voidedBody(t, "VICTIM", "GPA.1", clockNow.UnixMilli()))

	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestVoided_LatestOrderIsRefunded(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "VICTIM").Return(&google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateExpired,
		LineItems:         []google.LineItem{{ProductID: "p", LatestSuccessfulOrderID: "GPA.2"}},
	}, nil)

	ev, err := newParser(t, f, atClock()).Parse(context.Background(), voidedBody(t, "VICTIM", "GPA.2", clockNow.UnixMilli()))

	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRefunded, ev.State)
	assert.Equal(t, "VICTIM", ev.OriginalTxID)
}

// Without order ids Google cannot confirm which order was voided: only a
// subscription that no longer grants access is refunded.
func TestVoided_UnknownOrderNeedsNonGrantingState(t *testing.T) {
	t.Parallel()
	for state, wantRefund := range map[google.State]bool{
		google.StateActive: false, google.StateInGracePeriod: false, google.StateCanceled: false,
		google.StateExpired: true, google.StateOnHold: true, google.StatePaused: true,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "T").Return(&google.SubscriptionPurchaseV2{SubscriptionState: state}, nil)

			ev, err := newParser(t, f, atClock()).Parse(context.Background(), voidedBody(t, "T", "GPA.1", clockNow.UnixMilli()))

			if wantRefund {
				require.NoError(t, err)
				assert.Equal(t, entitlements.StateRefunded, ev.State)
			} else {
				assert.ErrorIs(t, err, entitlements.ErrIgnored)
			}
		})
	}
}

func TestVoided_FetchErrorsKeepTheirClass(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "GONE").Return(nil, fmt.Errorf("x: %w", google.ErrPurchaseNotFound))

	_, err := newParser(t, f, atClock()).Parse(context.Background(), voidedBody(t, "GONE", "o", clockNow.UnixMilli()))

	assert.ErrorIs(t, err, google.ErrPurchaseNotFound)
}

// Security H1: a far-future eventTimeMillis would freeze a chain.
func TestEventTimeInTheFutureIsMalformed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		offset time.Duration
		opts   []google.Option
		ok     bool
	}{
		"year 2100":              {offset: 24 * 365 * 70 * time.Hour, ok: false},
		"6 minutes ahead":        {offset: 6 * time.Minute, ok: false},
		"4 minutes ahead":        {offset: 4 * time.Minute, ok: true},
		"custom skew allows 1h":  {offset: time.Hour, opts: []google.Option{google.WithMaxFutureSkew(2 * time.Hour)}, ok: true},
		"custom skew rejects 3h": {offset: 3 * time.Hour, opts: []google.Option{google.WithMaxFutureSkew(2 * time.Hour)}, ok: false},
		"past is always fine":    {offset: -24 * time.Hour, ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "T").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil).Maybe()
			body := voidedBody(t, "T", "o", clockNow.Add(tc.offset).UnixMilli())

			_, err := newParser(t, f, append([]google.Option{atClock()}, tc.opts...)...).Parse(context.Background(), body)

			if tc.ok {
				assert.NotErrorIs(t, err, google.ErrMalformed)
			} else {
				assert.ErrorIs(t, err, google.ErrMalformed)
			}
		})
	}
}

// Security M3: control characters in ids break Postgres (NUL) and logs.
func TestControlCharsInIDsAreMalformed(t *testing.T) {
	t.Parallel()
	bad := []string{"a\x00b", "a\nb", "a\x7fb", "a\u0085b"}
	for _, v := range bad {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			t.Parallel()
			sub := fmt.Sprintf(`{"packageName":%q,"eventTimeMillis":"%d","subscriptionNotification":{"notificationType":4,"purchaseToken":%q,"subscriptionId":"p"}}`, pkg, clockNow.UnixMilli(), v)
			void := voidedBody(t, v, "o", clockNow.UnixMilli())
			f := mocks.NewMockSubscriptionsFetcher(t)
			p := newParser(t, f, atClock())

			_, err := p.Parse(context.Background(), testutil.GooglePushBody(t, "m", []byte(sub)))
			require.ErrorIs(t, err, google.ErrMalformed, "subscription token")
			_, err = p.Parse(context.Background(), void)
			require.ErrorIs(t, err, google.ErrMalformed, "voided token")
			_, err = p.Parse(context.Background(), testutil.GooglePushBody(t, v, []byte(sub)))
			assert.ErrorIs(t, err, google.ErrMalformed, "message id")
		})
	}
}

// Security M4: the package pin is mandatory.
func TestNewParser_PackageNameRequired(t *testing.T) {
	t.Parallel()
	p, err := google.NewParser(mocks.NewMockSubscriptionsFetcher(t), "")
	require.ErrorIs(t, err, google.ErrPackageRequired)
	assert.Nil(t, p)
}

func TestWrongPackageNeverReachesFetcher(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t) // any call fails the test
	_, err := newParser(t, f, atClock()).Parse(context.Background(), testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_wrong_package.json")))
	assert.ErrorIs(t, err, google.ErrPackageMismatch)
}

// Security M2: tokens must not leak through error text.
func TestFetchErrorTextHasNoToken(t *testing.T) {
	t.Parallel()
	const token = "SECRETPURCHASETOKEN123"
	underlying := fmt.Errorf(`Get "https://x/tokens/%s": dial tcp: refused`, token)
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, token).Return(nil, underlying)
	payload := fmt.Sprintf(`{"packageName":%q,"eventTimeMillis":"%d","subscriptionNotification":{"notificationType":4,"purchaseToken":%q,"subscriptionId":"p"}}`, pkg, clockNow.UnixMilli(), token)

	_, err := newParser(t, f, atClock()).Parse(context.Background(), testutil.GooglePushBody(t, "m", []byte(payload)))

	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
	require.ErrorIs(t, err, google.ErrFetchTransient)
	assert.ErrorIs(t, err, underlying, "original error stays reachable")
}

func TestRedactToken(t *testing.T) {
	t.Parallel()
	require.NoError(t, google.RedactToken(nil, "t"))
	err := errors.New("a tok b tok")
	got := google.RedactToken(err, "tok")
	require.Error(t, got)
	assert.Equal(t, "a [redacted] b [redacted]", got.Error())
	require.ErrorIs(t, got, err)
	assert.Same(t, err, errors.Unwrap(got))
	require.ErrorIs(t, google.RedactToken(err, ""), err, "empty token leaves the error alone")
	assert.Equal(t, "a tok b tok", google.RedactToken(err, "absent").Error(), "no token in text leaves the error alone")
}

// Domain 3: expiryTime in grace is already the grace end.
func TestGrace_TrustsExpiryTime(t *testing.T) {
	t.Parallel()
	occurred := time.UnixMilli(1760436000000).UTC()
	for name, tc := range map[string]struct {
		expiry time.Time
		want   time.Time
		err    error
	}{
		"future grace end":         {expiry: occurred.Add(3 * 24 * time.Hour), want: occurred.Add(3 * 24 * time.Hour)},
		"grace end already passed": {expiry: occurred.Add(-time.Hour), want: occurred.Add(-time.Hour)},
		"no expiry in grace":       {err: google.ErrNoExpiry},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev, err := parseWith(t, graceSub(tc.expiry), atClock())
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateGrace, ev.State)
			assert.Equal(t, tc.want, ev.ExpiresAt, "no floor, no extension")
			ent := entitlements.Entitlement{State: ev.State, ExpiresAt: ev.ExpiresAt}
			assert.Equal(t, tc.expiry.After(occurred), ent.Active(occurred))
		})
	}
}

// Domain 4: supersede only when the new purchase grants access.
func TestSupersedeOnlyWhenNewStateGrantsAccess(t *testing.T) {
	t.Parallel()
	exp := clockNow.Add(24 * time.Hour)
	cases := map[string]struct {
		state      google.State
		supersedes string
	}{
		"active":                    {google.StateActive, "old-token"},
		"canceled still has access": {google.StateCanceled, "old-token"},
		"grace":                     {google.StateInGracePeriod, "old-token"},
		"pending purchase canceled": {google.StatePendingCanceled, ""},
		"expired":                   {google.StateExpired, ""},
		"on hold":                   {google.StateOnHold, ""},
		"paused":                    {google.StatePaused, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev, err := parseWith(t, &google.SubscriptionPurchaseV2{
				SubscriptionState: tc.state, LinkedPurchaseToken: "old-token",
				LineItems: []google.LineItem{{ProductID: "p", ExpiryTime: exp}},
			}, atClock())
			require.NoError(t, err)
			assert.Equal(t, tc.supersedes, ev.SupersedesTxID)
		})
	}
}

func TestTestNotificationHasItsOwnSentinel(t *testing.T) {
	t.Parallel()
	body := testutil.GooglePushBody(t, "m", testutil.GoogleFixture(t, "notif_test.json"))
	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t), atClock()).Parse(context.Background(), body)
	require.ErrorIs(t, err, google.ErrTestNotification)
	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestTypedStates(t *testing.T) {
	t.Parallel()
	s := google.StateActive
	assert.True(t, strings.HasPrefix(string(s), "SUBSCRIPTION_STATE_"))
}
