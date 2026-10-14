package apple_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

var pT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// appleN builds a verified notification as the verifier would hand it to ToEvent.
func appleN(typ, sub string, signed time.Time, status int, tx apple.JWSTransactionDecodedPayload, ren *apple.JWSRenewalInfoDecodedPayload) apple.Notification {
	tx.OriginalTransactionID, tx.ProductID = "otid1", "pro.monthly"
	if tx.Type == "" {
		tx.Type = "Auto-Renewable Subscription"
	}
	return apple.Notification{
		Payload: apple.NotificationPayload{
			NotificationType: typ, Subtype: sub, NotificationUUID: typ + sub + signed.String(), SignedDate: signed.UnixMilli(),
			Data: &apple.NotificationData{Status: status},
		},
		Transaction: &tx, Renewal: ren,
	}
}

func ptr[T any](v T) *T { return &v }

func TestMapper_StatusIsThePrimaryStateForAmbiguousTypes(t *testing.T) {
	exp := pT0.Add(10 * 24 * time.Hour).UnixMilli()
	grace := pT0.Add(16 * 24 * time.Hour).UnixMilli()
	ren := &apple.JWSRenewalInfoDecodedPayload{GracePeriodExpiresDate: grace}
	tests := []struct {
		status int
		want   entitlements.State
	}{
		{1, entitlements.StateActive},
		{2, entitlements.StateExpired},
		{3, entitlements.StateBillingRetry},
		{4, entitlements.StateGrace},
		{5, entitlements.StateRevoked},
	}
	for _, typ := range []string{"DID_CHANGE_RENEWAL_STATUS", "DID_CHANGE_RENEWAL_PREF"} {
		for _, tc := range tests {
			t.Run(typ+"/"+string(tc.want), func(t *testing.T) {
				ev, err := apple.ToEvent(ptr(appleN(typ, "", pT0, tc.status, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, ren)))
				require.NoError(t, err)
				assert.Equal(t, tc.want, ev.State)
			})
		}
	}
	// no status: fall back to the expiry comparison
	ev, err := apple.ToEvent(ptr(appleN("DID_CHANGE_RENEWAL_STATUS", "", pT0, 0, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, ev.State)
}

func TestMapper_RevocationDateWinsOverType(t *testing.T) {
	exp := pT0.Add(10 * 24 * time.Hour).UnixMilli()
	rev := pT0.Add(-time.Hour).UnixMilli()
	for _, typ := range []string{"DID_RENEW", "DID_CHANGE_RENEWAL_STATUS", "DID_CHANGE_RENEWAL_PREF", "RENEWAL_EXTENDED"} {
		ev, err := apple.ToEvent(ptr(appleN(typ, "", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: rev}, nil)))
		require.NoError(t, err, typ)
		assert.Equal(t, entitlements.StateRefunded, ev.State, typ)
	}
	ev, err := apple.ToEvent(ptr(appleN("REVOKE", "", pT0, 5, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: rev}, nil)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateRevoked, ev.State)
	// REFUND_REVERSED carries the old revocationDate in some payloads; it must still restore.
	ev, err = apple.ToEvent(ptr(appleN("REFUND_REVERSED", "", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: rev}, nil)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, ev.State)
}

func TestMapper_ZeroExpiryNeverGrantsAccess(t *testing.T) {
	for _, tc := range []struct{ typ, sub string }{
		{"SUBSCRIBED", "INITIAL_BUY"}, {"DID_RENEW", ""}, {"OFFER_REDEEMED", ""},
		{"DID_CHANGE_RENEWAL_PREF", "UPGRADE"}, {"RENEWAL_EXTENDED", ""},
	} {
		_, err := apple.ToEvent(ptr(appleN(tc.typ, tc.sub, pT0, 1, apple.JWSTransactionDecodedPayload{}, nil)))
		require.ErrorIs(t, err, entitlements.ErrUnsupported, tc.typ)
		require.ErrorIs(t, err, apple.ErrNoExpiry, tc.typ)
	}
	// grace without a grace end is refused too: it would deny silently
	_, err := apple.ToEvent(ptr(appleN("DID_FAIL_TO_RENEW", "GRACE_PERIOD", pT0, 4, apple.JWSTransactionDecodedPayload{ExpiresDate: pT0.UnixMilli()}, nil)))
	require.ErrorIs(t, err, entitlements.ErrUnsupported)
	// states without access need no expiry
	ev, err := apple.ToEvent(ptr(appleN("EXPIRED", "VOLUNTARY", pT0, 2, apple.JWSTransactionDecodedPayload{}, nil)))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, ev.State)
}

func TestMapper_NonStateTypesAreIgnoredNotUnsupported(t *testing.T) {
	for _, typ := range []string{"REFUND_DECLINED", "PRICE_INCREASE", "CONSUMPTION_REQUEST", "EXTERNAL_PURCHASE_TOKEN", "METADATA_UPDATE", "MIGRATION", "PRICE_CHANGE", "RESCIND_CONSENT", "CHILD_CONSENT_REVOKED"} {
		_, err := apple.ToEvent(ptr(appleN(typ, "", pT0, 1, apple.JWSTransactionDecodedPayload{}, nil)))
		assert.ErrorIs(t, err, entitlements.ErrIgnored, typ)
	}
	_, err := apple.ToEvent(ptr(appleN("SOMETHING_NEW", "", pT0, 1, apple.JWSTransactionDecodedPayload{}, nil)))
	assert.ErrorIs(t, err, entitlements.ErrUnsupported)
}

func TestMapper_RefundOfNonSubscriptionIsIgnored(t *testing.T) {
	for _, typ := range []string{"REFUND", "REVOKE"} {
		_, err := apple.ToEvent(ptr(appleN(typ, "", pT0, 5, apple.JWSTransactionDecodedPayload{Type: "Consumable"}, nil)))
		assert.ErrorIs(t, err, entitlements.ErrIgnored, typ)
	}
}

func TestMapper_EnvironmentMapping(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	tests := []struct {
		name         string
		txEnv, outer string
		want         entitlements.Environment
		sandbox      bool
	}{
		{"production", "Production", "Production", entitlements.EnvironmentProduction, false},
		{"sandbox", "Sandbox", "Sandbox", entitlements.EnvironmentSandbox, true},
		{"xcode", "Xcode", "", entitlements.EnvironmentXcode, true},
		{"local testing", "LocalTesting", "", entitlements.EnvironmentLocalTesting, true},
		{"tx empty falls back to the outer environment", "", "Sandbox", entitlements.EnvironmentSandbox, true},
		{"tx wins over the outer one", "Production", "Sandbox", entitlements.EnvironmentProduction, false},
		{"both empty is production", "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := appleN("SUBSCRIBED", "INITIAL_BUY", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, Environment: tc.txEnv}, nil)
			n.Payload.Data.Environment = tc.outer
			ev, err := apple.ToEvent(&n)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ev.Environment)
			assert.Equal(t, tc.sandbox, ev.Sandbox())
		})
	}
}

func TestMapper_MapperBoundaries(t *testing.T) {
	t.Run("equal expiry and signedDate without status is expired", func(t *testing.T) {
		ev, err := apple.ToEvent(ptr(appleN("DID_CHANGE_RENEWAL_STATUS", "", pT0, 0, apple.JWSTransactionDecodedPayload{ExpiresDate: pT0.UnixMilli()}, nil)))
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateExpired, ev.State)
	})
	t.Run("grace end earlier than expiresDate does not shorten access", func(t *testing.T) {
		exp := pT0.Add(10 * 24 * time.Hour)
		ren := &apple.JWSRenewalInfoDecodedPayload{GracePeriodExpiresDate: pT0.Add(24 * time.Hour).UnixMilli()}
		ev, err := apple.ToEvent(ptr(appleN("DID_FAIL_TO_RENEW", "GRACE_PERIOD", pT0, 4, apple.JWSTransactionDecodedPayload{ExpiresDate: exp.UnixMilli()}, ren)))
		require.NoError(t, err)
		assert.True(t, exp.Equal(ev.ExpiresAt), ev.ExpiresAt)
	})
	t.Run("a subtype other than GRACE_PERIOD is billing retry", func(t *testing.T) {
		for _, sub := range []string{"", "BILLING_RETRY", "PRICE_INCREASE", "grace_period"} {
			ev, err := apple.ToEvent(ptr(appleN("DID_FAIL_TO_RENEW", sub, pT0, 3, apple.JWSTransactionDecodedPayload{ExpiresDate: pT0.UnixMilli()}, nil)))
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateBillingRetry, ev.State, sub)
		}
	})
	t.Run("a missing expiresDate stays the zero time", func(t *testing.T) {
		ev, err := apple.ToEvent(ptr(appleN("EXPIRED", "VOLUNTARY", pT0, 2, apple.JWSTransactionDecodedPayload{}, nil)))
		require.NoError(t, err)
		assert.True(t, ev.ExpiresAt.IsZero(), ev.ExpiresAt)
	})
}

func TestMapper_RefundTransactionTypeFilter(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	rev := pT0.UnixMilli()
	for _, typ := range []string{"REFUND", "REVOKE"} {
		for txType, ignored := range map[string]bool{
			"Auto-Renewable Subscription": false,
			"":                            false, // unknown type: treat as a subscription
			"Consumable":                  true,
			"Non-Consumable":              true,
			"Non-Renewing Subscription":   true,
		} {
			tx := apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: rev, Type: txType}
			n := appleN(typ, "", pT0, 5, tx, nil)
			n.Transaction.Type = txType // appleN defaults an empty type to a subscription
			_, err := apple.ToEvent(&n)
			if ignored {
				assert.ErrorIs(t, err, entitlements.ErrIgnored, typ+"/"+txType)
			} else {
				assert.NoError(t, err, typ+"/"+txType)
			}
		}
	}
	// other types are never filtered by transaction type
	n := appleN("DID_RENEW", "", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil)
	n.Transaction.Type = "Consumable"
	_, err := apple.ToEvent(&n)
	assert.NoError(t, err)
}

func TestMapper_NotificationWithoutDataObject(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	n := appleN("DID_CHANGE_RENEWAL_STATUS", "", pT0, 0, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, Environment: "Sandbox"}, nil)
	n.Payload.Data = nil
	ev, err := apple.ToEvent(&n)
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, ev.State, "no status: the expiry decides")
	assert.Equal(t, entitlements.EnvironmentSandbox, ev.Environment)

	n.Transaction.Environment = ""
	ev, err = apple.ToEvent(&n)
	require.NoError(t, err)
	assert.Equal(t, entitlements.Environment(""), ev.Environment, "nothing to fall back to")
}

func TestMapper_RevocationDateOnFixedTypes(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	rev := pT0.Add(-time.Hour).UnixMilli()
	for typ, want := range map[string]entitlements.State{
		"SUBSCRIBED":           entitlements.StateRefunded,
		"DID_RENEW":            entitlements.StateRefunded,
		"EXPIRED":              entitlements.StateExpired,
		"GRACE_PERIOD_EXPIRED": entitlements.StateBillingRetry,
		"REFUND":               entitlements.StateRefunded,
		"REVOKE":               entitlements.StateRevoked,
	} {
		ev, err := apple.ToEvent(ptr(appleN(typ, "", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: rev}, nil)))
		require.NoError(t, err, typ)
		assert.Equal(t, want, ev.State, typ)
		ev, err = apple.ToEvent(ptr(appleN(typ, "", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil)))
		require.NoError(t, err, typ)
		if typ == "SUBSCRIBED" || typ == "DID_RENEW" {
			assert.Equal(t, entitlements.StateActive, ev.State, "without revocationDate "+typ)
		}
	}
	ev, err := apple.ToEvent(ptr(appleN("SOMETHING", "", pT0, 1, apple.JWSTransactionDecodedPayload{}, nil)))
	require.Error(t, err)
	assert.Equal(t, entitlements.Event{}, ev)
}

func TestMapper_ReviveFlagsPerType(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	tests := []struct {
		typ      string
		sub      string
		noRevive bool
		restores bool
	}{
		{"SUBSCRIBED", "RESUBSCRIBE", false, false},
		{"DID_RENEW", "", false, false},
		{"REFUND_REVERSED", "", false, true},
		{"DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", true, false},
		{"DID_CHANGE_RENEWAL_PREF", "UPGRADE", true, false},
		{"OFFER_REDEEMED", "", true, false},
		{"RENEWAL_EXTENDED", "", true, false},
		{"DID_FAIL_TO_RENEW", "", false, false},
		{"EXPIRED", "VOLUNTARY", false, false},
		{"REFUND", "", false, false},
		{"REVOKE", "", false, false},
	}
	for _, tc := range tests {
		ev, err := apple.ToEvent(ptr(appleN(tc.typ, tc.sub, pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil)))
		require.NoError(t, err, tc.typ)
		assert.Equal(t, tc.noRevive, ev.NoRevive, tc.typ+" NoRevive")
		assert.Equal(t, tc.restores, ev.Restores, tc.typ+" Restores")
	}
}

func TestMapper_AutoRenew(t *testing.T) {
	exp := pT0.Add(24 * time.Hour).UnixMilli()
	one, zero := 1, 0
	tests := []struct {
		name string
		typ  string
		sub  string
		ren  *apple.JWSRenewalInfoDecodedPayload
		want *bool
	}{
		{"renewal info on", "DID_RENEW", "", &apple.JWSRenewalInfoDecodedPayload{AutoRenewStatus: &one}, ptr(true)},
		{"renewal info off", "DID_RENEW", "", &apple.JWSRenewalInfoDecodedPayload{AutoRenewStatus: &zero}, ptr(false)},
		{"renewal info without the field", "DID_RENEW", "", &apple.JWSRenewalInfoDecodedPayload{}, nil},
		{"no renewal info", "DID_RENEW", "", nil, nil},
		{"subtype enabled", "DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_ENABLED", nil, ptr(true)},
		{"subtype disabled", "DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", nil, ptr(false)},
		{"renewal info wins over the subtype", "DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", &apple.JWSRenewalInfoDecodedPayload{AutoRenewStatus: &one}, ptr(true)},
		{"subtype of another type is not a setting", "DID_RENEW", "AUTO_RENEW_DISABLED", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := apple.ToEvent(ptr(appleN(tc.typ, tc.sub, pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, tc.ren)))
			require.NoError(t, err)
			assert.Equal(t, tc.want, ev.AutoRenew)
		})
	}
}
