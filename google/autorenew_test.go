package google_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func renewing(product string, on bool) google.LineItem {
	li := item(product, tBase)
	li.AutoRenewingPlan = &google.AutoRenewingPlan{AutoRenewEnabled: on}
	return li
}

func TestAutoRenew_PerItem(t *testing.T) {
	t.Parallel()
	prepaid := item("prepaid", tAddon) // no autoRenewingPlan: unknown, not false
	f := fetcherFor(t, fetchMap{"tok": sub(google.StateActive, renewing("base", true), renewing("addon", false), prepaid)}, nil)

	evs, err := newParser(t, f, atClock()).ParseAll(context.Background(), addonBody(t, "tok"))

	require.NoError(t, err)
	m := byKey(evs)
	require.NotNil(t, m["tok#base"].AutoRenew)
	assert.True(t, *m["tok#base"].AutoRenew)
	require.NotNil(t, m["tok#addon"].AutoRenew)
	assert.False(t, *m["tok#addon"].AutoRenew)
	assert.Nil(t, m["tok#prepaid"].AutoRenew)
}

func TestAutoRenew_SingleItemAndCanceledState(t *testing.T) {
	t.Parallel()
	// CANCELED: access until expiry, auto-renew off.
	f := fetcherFor(t, fetchMap{"tok-1": sub(google.StateCanceled, renewing("premium.monthly", false))}, nil)

	ev, err := newParser(t, f, atClock()).Parse(context.Background(), subBody(t))

	require.NoError(t, err)
	require.NotNil(t, ev.AutoRenew)
	assert.False(t, *ev.AutoRenew)
}

func TestAutoRenew_ParsedFromTheAPIShape(t *testing.T) {
	t.Parallel()
	var s google.SubscriptionPurchaseV2
	require.NoError(t, jsonUnmarshal(testutil.PriorArtFixture(t, priorGoogle+"sub_active_obfuscated_account_only.json"), &s))
	require.Len(t, s.LineItems, 1)
	require.NotNil(t, s.LineItems[0].AutoRenewingPlan)
	assert.True(t, s.LineItems[0].AutoRenewingPlan.AutoRenewEnabled)
}

func TestAutoRenew_NilWithoutAPlan(t *testing.T) {
	t.Parallel()
	ev, err := parseWith(t, activeItems(item("p", tBase)))
	require.NoError(t, err)
	assert.Nil(t, ev.AutoRenew)
}
