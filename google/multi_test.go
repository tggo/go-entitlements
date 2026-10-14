package google_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/google"
)

// Subscriptions with add-ons: one purchase token, several line items, each
// with its own expiryTime
// (https://developer.android.com/google/play/billing/subscription-with-addons,
// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2).
// The RTDN of such a purchase carries no subscriptionId.

var (
	tBase  = clockNow.Add(30 * 24 * time.Hour)
	tAddon = clockNow.Add(10 * 24 * time.Hour)
	tPast  = clockNow.Add(-48 * time.Hour)
)

func item(product string, expiry time.Time) google.LineItem {
	return google.LineItem{ProductID: product, ExpiryTime: expiry}
}

func sub(state google.State, items ...google.LineItem) *google.SubscriptionPurchaseV2 {
	return &google.SubscriptionPurchaseV2{
		SubscriptionState:          state,
		LineItems:                  items,
		ExternalAccountIdentifiers: &google.ExternalAccountIdentifiers{ObfuscatedExternalAccountID: "user-1"},
	}
}

// addonBody is a push without subscriptionId, as Play sends for add-ons.
func addonBody(tb testing.TB, token string) []byte {
	tb.Helper()
	return pushOf(tb, fmt.Sprintf(`{"packageName":%q,"eventTimeMillis":"%d","subscriptionNotification":{"notificationType":4,"purchaseToken":%q}}`,
		pkg, clockNow.UnixMilli(), token))
}

type fetchMap map[string]*google.SubscriptionPurchaseV2

// fetcherFor answers GetV2 from m (error values for tokens mapped to nil are
// supplied by errs) and fails the test on a token it does not know.
func fetcherFor(t *testing.T, m fetchMap, errs map[string]error) *mocks.MockSubscriptionsFetcher {
	t.Helper()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, mock.Anything).RunAndReturn(
		func(_ context.Context, _, token string) (*google.SubscriptionPurchaseV2, error) {
			if err, ok := errs[token]; ok {
				return nil, err
			}
			s, ok := m[token]
			if !ok {
				t.Errorf("unexpected fetch of %q", token)
				return nil, fmt.Errorf("unexpected fetch of %q", token)
			}
			return s, nil
		}).Maybe()
	return f
}

func byKey(evs []entitlements.Event) map[string]entitlements.Event {
	m := map[string]entitlements.Event{}
	for _, e := range evs {
		m[e.OriginalTxID] = e
	}
	return m
}

func TestParseAll_AddOnsBecomeOneEntitlementPerItem(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("base", tBase), item("addon", tAddon))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	require.Len(t, evs, 2)
	m := byKey(evs)
	base, addon := m["tok#base"], m["tok#addon"]
	assert.Equal(t, "base", base.ProductID)
	assert.Equal(t, tBase, base.ExpiresAt)
	assert.Equal(t, "addon", addon.ProductID)
	assert.Equal(t, tAddon, addon.ExpiresAt)
	assert.Equal(t, "m#base", base.ID)
	assert.Equal(t, "m#addon", addon.ID)
	for _, e := range evs {
		assert.Equal(t, entitlements.StateActive, e.State)
		assert.Equal(t, "user-1", e.UserID)
		assert.Equal(t, clockNow.Truncate(time.Millisecond), e.OccurredAt.Truncate(time.Millisecond))
		assert.Empty(t, e.SupersedesTxID)
	}
}

func TestParseAll_SingleItemKeepsTheLegacyKeyAndID(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok-1": sub(google.StateActive, item("premium.monthly", tBase))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), subBody(t))

	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "tok-1", evs[0].OriginalTxID)
	assert.Equal(t, "m", evs[0].ID)
}

func TestParseAll_DeferredReplacementIsOneEntitlement(t *testing.T) {
	t.Parallel()
	current := item("pro.monthly", tAddon)
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, current, item("pro.yearly", time.Time{}))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	require.Len(t, evs, 1, "the replacement has no expiryTime yet and grants nothing")
	assert.Equal(t, "tok", evs[0].OriginalTxID)
	assert.Equal(t, "pro.monthly", evs[0].ProductID)
}

// After the replacement the old item has a past expiryTime and the new one a
// future one.
func TestParseAll_ReplacementTakesEffect(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("pro.monthly", tPast), item("pro.yearly", tBase))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "pro.yearly", evs[0].ProductID)
	assert.Equal(t, tBase, evs[0].ExpiresAt)
}

// Removed add-ons "expire at the end of their current billing periods": the
// item either disappears or carries a past expiryTime. Either way its stored
// row already ends at that time, so no event is emitted for it.
func TestParseAll_LapsedAddOnEmitsNothing(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("base", tBase), item("addon", tPast))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "base", evs[0].ProductID)
}

func TestParseAll_ActiveWithAllItemsLapsedIsRefused(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("base", tPast), item("addon", tPast))}, nil)

	_, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	assert.ErrorIs(t, err, google.ErrNoExpiry)
}

func TestParseAll_NonGrantingStateCoversEveryItem(t *testing.T) {
	t.Parallel()
	for state, want := range map[google.State]entitlements.State{
		google.StateExpired: entitlements.StateExpired, google.StateOnHold: entitlements.StateBillingRetry,
		google.StatePaused: entitlements.StatePaused,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := fetcherFor(t, fetchMap{"tok": sub(state, item("base", tPast), item("addon", time.Time{}))}, nil)

			evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

			require.NoError(t, err)
			require.Len(t, evs, 2)
			for _, e := range evs {
				assert.Equal(t, want, e.State)
			}
		})
	}
}

func TestParseAll_GraceKeepsEveryItemWithItsOwnExpiry(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateInGracePeriod, item("base", tBase), item("addon", tAddon))}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	m := byKey(evs)
	assert.Equal(t, tBase, m["tok#base"].ExpiresAt)
	assert.Equal(t, tAddon, m["tok#addon"].ExpiresAt)
	assert.Equal(t, entitlements.StateGrace, m["tok#base"].State)
}

func TestParse_RefusesWhatNeedsSeveralEvents(t *testing.T) {
	t.Parallel()
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, item("base", tBase), item("addon", tAddon))}, nil)

	_, err := newParser(t, f, atClock()).Parse(context.Background(), addonBody(t, "tok"))

	assert.ErrorIs(t, err, google.ErrMultipleEvents)
}

func TestAck_AddOnSubscriptionUsesAnItemProductWhenRTDNHasNoSubscriptionID(t *testing.T) {
	t.Parallel()
	s := sub(google.StateActive, item("base", tBase), item("addon", tAddon))
	s.AcknowledgementState = ackPending
	f := fetcherFor(t, fetchMap{"tok": s}, nil)

	res, err := newParser(t, f, atClock()).ParseResult(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	require.NotNil(t, res.Ack)
	assert.Equal(t, google.Ack{PackageName: pkg, SubscriptionID: "base", PurchaseToken: "tok"}, *res.Ack)
}

// Upgrade carrying add-ons: the new token supersedes every key of the old one.
func TestParseAll_UpgradeRetiresEveryItemOfTheOldToken(t *testing.T) {
	t.Parallel()
	newer := sub(google.StateActive, item("base2", tBase), item("addon", tAddon))
	newer.LinkedPurchaseToken = "old"
	f := fetcherFor(t, fetchMap{
		"new": newer,
		"old": sub(google.StateExpired, item("base", tPast), item("addon", tPast)),
	}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "new"))

	require.NoError(t, err)
	m := byKey(evs)
	require.Len(t, evs, 4, "two items of the new token, two retire events for the old one")
	assert.Equal(t, entitlements.StateActive, m["new#base2"].State)
	assert.Equal(t, "old", m["new#base2"].SupersedesTxID, "the first granting event keeps the legacy supersede link")
	for _, key := range []string{"old#base", "old#addon"} {
		assert.Equal(t, entitlements.StateExpired, m[key].State, key)
		assert.Equal(t, entitlements.SourceGoogle, m[key].Source)
		assert.NotEmpty(t, m[key].ID)
	}
	ids := map[string]bool{}
	for _, e := range evs {
		assert.False(t, ids[e.ID], "event ids are unique: %s", e.ID)
		ids[e.ID] = true
	}
}

// Multi-hop: only C arrives (linked B, B linked A). A must be retired too.
func TestParseAll_MultiHopLinkRetiresTheWholeChain(t *testing.T) {
	t.Parallel()
	c := sub(google.StateActive, item("p", tBase))
	c.LinkedPurchaseToken = "B"
	b := sub(google.StateExpired, item("p", tPast))
	b.LinkedPurchaseToken = "A"
	a := sub(google.StateExpired, item("p", tPast))
	f := fetcherFor(t, fetchMap{"C": c, "B": b, "A": a}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "C"))

	require.NoError(t, err)
	m := byKey(evs)
	require.Len(t, evs, 2)
	assert.Equal(t, "B", m["C"].SupersedesTxID)
	assert.Equal(t, entitlements.StateExpired, m["A"].State)
}

func TestParseAll_LinkChainIsBounded(t *testing.T) {
	t.Parallel()
	subs := fetchMap{}
	for i := 0; i < 20; i++ {
		s := sub(google.StateExpired, item("p", tPast))
		s.LinkedPurchaseToken = fmt.Sprintf("t%d", i+1)
		subs[fmt.Sprintf("t%d", i)] = s
	}
	subs["t0"].SubscriptionState = google.StateActive
	subs["t0"].LineItems = []google.LineItem{item("p", tBase)}
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, mock.Anything).RunAndReturn(
		func(_ context.Context, _, token string) (*google.SubscriptionPurchaseV2, error) {
			return subs[token], nil
		}).Times(1 + google.MaxLinkedTokens)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "t0"))

	require.NoError(t, err)
	assert.Len(t, evs, google.MaxLinkedTokens, "the new token plus the retire events of at most MaxLinkedTokens replaced tokens (the first through the supersede link)")
}

func TestParseAll_LinkCycleTerminates(t *testing.T) {
	t.Parallel()
	a := sub(google.StateActive, item("p", tBase))
	a.LinkedPurchaseToken = "B"
	b := sub(google.StateExpired, item("p", tPast))
	b.LinkedPurchaseToken = "A"
	f := fetcherFor(t, fetchMap{"A": a, "B": b}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "A"))

	require.NoError(t, err)
	assert.Len(t, evs, 1, "A links B links A: A is never retired by itself")
}

func TestParseAll_UnreadableOldTokenFallsBackToTheLegacyKey(t *testing.T) {
	t.Parallel()
	for name, errClass := range map[string]error{
		"not found": google.ErrPurchaseNotFound, "invalid": google.ErrPurchaseInvalid,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			newer := sub(google.StateActive, item("p", tBase))
			newer.LinkedPurchaseToken = "old"
			f := fetcherFor(t, fetchMap{"new": newer}, map[string]error{"old": fmt.Errorf("x: %w", errClass)})

			evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "new"))

			require.NoError(t, err)
			require.Len(t, evs, 1)
			assert.Equal(t, "old", evs[0].SupersedesTxID)
		})
	}
}

func TestParseAll_TransientFailureOnTheOldTokenIsRetried(t *testing.T) {
	t.Parallel()
	for name, errClass := range map[string]error{
		"transient": google.ErrFetchTransient, "config": google.ErrFetchConfig,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			newer := sub(google.StateActive, item("p", tBase))
			newer.LinkedPurchaseToken = "old"
			f := fetcherFor(t, fetchMap{"new": newer}, map[string]error{"old": fmt.Errorf("x: %w", errClass)})

			_, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "new"))

			assert.ErrorIs(t, err, errClass)
		})
	}
}

func TestParseAll_NoLinkFetchWhenTheNewStateGrantsNothing(t *testing.T) {
	t.Parallel()
	cancelled := sub(google.StatePendingCanceled, item("p", tBase))
	cancelled.LinkedPurchaseToken = "old"
	f := fetcherFor(t, fetchMap{"new": cancelled}, nil) // a fetch of "old" fails the test

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "new"))

	require.NoError(t, err)
	for _, e := range evs {
		assert.Empty(t, e.SupersedesTxID)
	}
}

func TestParseAll_VoidedRefundsEveryItemAndTheLegacyKey(t *testing.T) {
	t.Parallel()
	s := sub(google.StateExpired,
		google.LineItem{ProductID: "base", LatestSuccessfulOrderID: "GPA.1"},
		google.LineItem{ProductID: "addon", LatestSuccessfulOrderID: "GPA.1"})
	f := fetcherFor(t, fetchMap{"T": s}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), voidedBody(t, "T", "GPA.1", clockNow.UnixMilli()))

	require.NoError(t, err)
	m := byKey(evs)
	require.Len(t, evs, 3)
	for _, key := range []string{"T", "T#base", "T#addon"} {
		assert.Equal(t, entitlements.StateRefunded, m[key].State, key)
	}
	assert.NotEqual(t, m["T#base"].ID, m["T#addon"].ID)
}

func TestParseAll_VoidedSingleItemKeepsTheLegacyKeyOnly(t *testing.T) {
	t.Parallel()
	s := sub(google.StateExpired, google.LineItem{ProductID: "p", LatestSuccessfulOrderID: "GPA.1"})
	f := fetcherFor(t, fetchMap{"T": s}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), voidedBody(t, "T", "GPA.1", clockNow.UnixMilli()))

	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "T", evs[0].OriginalTxID)
	assert.Equal(t, "msg-void", evs[0].ID)
}
