package entitlements_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/store/memory"
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

func runApple(t *testing.T, svc *entitlements.Service, n apple.Notification) (*entitlements.Entitlement, error) {
	t.Helper()
	ev, err := apple.ToEvent(&n)
	if err != nil {
		return nil, err
	}
	return svc.Process(context.Background(), ev)
}

func mustRunApple(t *testing.T, svc *entitlements.Service, n apple.Notification) *entitlements.Entitlement {
	t.Helper()
	e, err := runApple(t, svc, n)
	require.NoError(t, err)
	return e
}

func newMemSvc() *entitlements.Service { return entitlements.NewService(memory.New()) }

// Apple sends AUTO_RENEW_DISABLED after a refund request; it must not lift a refund.
func TestAppleDomain_RefundThenAutoRenewDisabledStaysRefunded(t *testing.T) {
	for name, revocation := range map[string]int64{"with revocationDate": pT0.Add(time.Hour).UnixMilli(), "without revocationDate": 0} {
		t.Run(name, func(t *testing.T) {
			svc := newMemSvc()
			exp := pT0.Add(20 * 24 * time.Hour).UnixMilli()
			mustRunApple(t, svc, appleN("SUBSCRIBED", "INITIAL_BUY", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil))
			mustRunApple(t, svc, appleN("REFUND", "", pT0.Add(time.Hour), 5, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: pT0.Add(time.Hour).UnixMilli()}, nil))
			e, _ := runApple(t, svc, appleN("DID_CHANGE_RENEWAL_STATUS", "AUTO_RENEW_DISABLED", pT0.Add(2*time.Hour), 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: revocation}, nil))
			got, err := memGet(svc)
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateRefunded, got.State, "event result %v", e)
			assert.False(t, got.Active(pT0.Add(3*time.Hour)))
		})
	}
}

func memGet(svc *entitlements.Service) (*entitlements.Entitlement, error) {
	return svc.Get(context.Background(), entitlements.SourceApple, "otid1")
}

// A resubscription or a renewal is a real purchase and may lift a refund.
func TestAppleDomain_OnlyPurchaseEventsLeaveRefunded(t *testing.T) {
	exp := pT0.Add(20 * 24 * time.Hour).UnixMilli()
	exp2 := pT0.Add(50 * 24 * time.Hour).UnixMilli()
	tests := []struct {
		typ, sub string
		want     entitlements.State
	}{
		{"SUBSCRIBED", "RESUBSCRIBE", entitlements.StateActive},
		{"DID_RENEW", "", entitlements.StateActive},
		{"OFFER_REDEEMED", "", entitlements.StateRefunded},
		{"DID_CHANGE_RENEWAL_PREF", "UPGRADE", entitlements.StateRefunded},
		{"RENEWAL_EXTENDED", "", entitlements.StateRefunded},
		{"DID_FAIL_TO_RENEW", "", entitlements.StateRefunded},
		{"EXPIRED", "", entitlements.StateRefunded},
	}
	for _, tc := range tests {
		t.Run(tc.typ+tc.sub, func(t *testing.T) {
			svc := newMemSvc()
			mustRunApple(t, svc, appleN("SUBSCRIBED", "INITIAL_BUY", pT0, 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, nil))
			mustRunApple(t, svc, appleN("REFUND", "", pT0.Add(time.Hour), 5, apple.JWSTransactionDecodedPayload{ExpiresDate: exp, RevocationDate: pT0.Add(time.Hour).UnixMilli()}, nil))
			_, _ = runApple(t, svc, appleN(tc.typ, tc.sub, pT0.Add(2*time.Hour), 1, apple.JWSTransactionDecodedPayload{ExpiresDate: exp2}, nil))
			got, err := memGet(svc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.State)
		})
	}
}

func TestAppleDomain_GraceSurvivesStatusAndPrefChanges(t *testing.T) {
	for _, typ := range []string{"DID_CHANGE_RENEWAL_STATUS", "DID_CHANGE_RENEWAL_PREF"} {
		t.Run(typ, func(t *testing.T) {
			svc := newMemSvc()
			exp := pT0.UnixMilli()
			grace := pT0.Add(16 * 24 * time.Hour).UnixMilli()
			ren := &apple.JWSRenewalInfoDecodedPayload{GracePeriodExpiresDate: grace, IsInBillingRetryPeriod: true}
			mustRunApple(t, svc, appleN("DID_FAIL_TO_RENEW", "GRACE_PERIOD", pT0.Add(time.Minute), 4, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, ren))
			e := mustRunApple(t, svc, appleN(typ, "", pT0.Add(time.Hour), 4, apple.JWSTransactionDecodedPayload{ExpiresDate: exp}, ren))
			assert.Equal(t, entitlements.StateGrace, e.State)
			assert.True(t, e.Active(pT0.Add(2*time.Hour)), "in-grace user keeps access")
			assert.True(t, time.UnixMilli(grace).Equal(e.ExpiresAt), "grace expiry is kept: %s", e.ExpiresAt)
		})
	}
}
