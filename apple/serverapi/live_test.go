//go:build live

package serverapi_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/apple/serverapi"
	"github.com/tggo/go-entitlements/store/memory"
)

// tail masks an identifier for test logs.
func tail(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[len(id)-4:]
}

// TestLiveSandbox calls the real Sandbox App Store Server API. It is skipped
// unless ASC_ISSUER_ID, ASC_KEY_ID, ASC_KEY_PATH and BUNDLE_ID are set:
//
//	ASC_ISSUER_ID=... ASC_KEY_ID=... ASC_KEY_PATH=/path/outside/the/repo/AuthKey_XXXX.p8 \
//	BUNDLE_ID=com.example.app go test -tags=live -run TestLiveSandbox -v ./apple/serverapi
//
// Get All Subscription Statuses, Get Transaction History and Reconcile (into an
// in-memory Service) run for ORIGINAL_TRANSACTION_ID, or for the customer of the
// first notification in the history. Only read-only calls and the
// harmless Request a Test Notification are made. It never sends consumption
// information: that would answer a real customer's refund request.
func TestLiveSandbox(t *testing.T) {
	issuer, keyID, keyPath, bundle := os.Getenv("ASC_ISSUER_ID"), os.Getenv("ASC_KEY_ID"), os.Getenv("ASC_KEY_PATH"), os.Getenv("BUNDLE_ID")
	if issuer == "" || keyID == "" || keyPath == "" || bundle == "" {
		t.Skip("set ASC_ISSUER_ID, ASC_KEY_ID, ASC_KEY_PATH and BUNDLE_ID to run against the real Sandbox")
	}
	key, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	c, err := serverapi.New(serverapi.Config{
		IssuerID: issuer, KeyID: keyID, PrivateKeyPEM: key, BundleID: bundle,
		Environment: entitlements.EnvironmentSandbox,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("request test notification", func(t *testing.T) {
		token, err := c.RequestTestNotification(ctx)
		if errors.Is(err, serverapi.ErrNotificationURLNotFound) {
			t.Skipf("authenticated, but no Sandbox notification URL is configured for %s: %v", bundle, err)
		}
		require.NoError(t, err)
		t.Logf("test notification token received (%d chars)", len(token))

		var st *serverapi.TestNotificationStatus
		for range 10 {
			st, err = c.GetTestNotificationStatus(ctx, token)
			if !errors.Is(err, serverapi.ErrTestNotificationNotReady) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		require.NoError(t, err)
		t.Logf("first send attempt: %s, attempts: %d, verified: %v", st.FirstSendAttemptResult, len(st.SendAttempts), st.Notification != nil)
		if st.VerifyErr != nil {
			t.Logf("verify error: %v", st.VerifyErr)
		}
		require.NoError(t, st.VerifyErr, "the signed TEST notification must verify against Apple Root CA G3")
		require.Equal(t, "TEST", st.Notification.Payload.NotificationType)
	})

	t.Run("replay notification history", func(t *testing.T) {
		v, err := apple.NewVerifier(bundle, apple.WithEnvironment(entitlements.EnvironmentSandbox))
		require.NoError(t, err)
		svc := entitlements.NewService(memory.New(), entitlements.WithAllowSandbox())
		rep, err := serverapi.ReplayNotificationHistory(ctx, c, apple.NewParser(v), svc, time.Now().Add(-29*24*time.Hour), time.Now())
		t.Logf("replay: total %d applied %d duplicate %d stale %d ignored %d failed %d", rep.Total, rep.Applied, rep.Duplicate, rep.Stale, rep.Ignored, rep.Failed)
		require.NoError(t, err)
	})

	foundTx := os.Getenv("ORIGINAL_TRANSACTION_ID")
	t.Run("notification history", func(t *testing.T) {
		n, verified := 0, 0
		for rec, err := range c.NotificationHistory(ctx, serverapi.NotificationHistoryRequest{
			Start: time.Now().Add(-29 * 24 * time.Hour), End: time.Now(),
		}) {
			require.NoError(t, err)
			n++
			if rec.Notification != nil {
				verified++
				if foundTx == "" && rec.Notification.Transaction != nil {
					foundTx = rec.Notification.Transaction.OriginalTransactionID
				}
				t.Logf("%s/%s first=%s", rec.Notification.Payload.NotificationType, rec.Notification.Payload.Subtype, rec.FirstSendAttemptResult)
			} else {
				t.Logf("unverified record: %v", rec.VerifyErr)
			}
			if n >= 40 {
				break
			}
		}
		t.Logf("%d records read, %d verified", n, verified)
	})

	if id := foundTx; id != "" {
		t.Run("statuses, history and reconcile", func(t *testing.T) {
			st, err := c.GetAllSubscriptionStatuses(ctx, id)
			require.NoError(t, err)
			for _, g := range st.Groups {
				for _, s := range g.Subscriptions {
					t.Logf("chain ...%s status %s product %s", tail(s.OriginalTransactionID), s.Status, s.Transaction.ProductID)
				}
			}
			n := 0
			for _, err := range c.TransactionHistory(ctx, id, serverapi.HistoryQuery{}) {
				require.NoError(t, err)
				n++
			}
			t.Logf("%d transactions in history", n)

			svc := entitlements.NewService(memory.New(), entitlements.WithAllowSandbox())
			for round := 1; round <= 2; round++ {
				res, err := serverapi.Reconcile(ctx, c, svc, id)
				require.NoError(t, err)
				for _, r := range res {
					t.Logf("reconcile round %d: chain ...%s -> %s (%s, expires %s)", round, tail(r.Event.OriginalTxID), r.Outcome, r.Event.State, r.Event.ExpiresAt.Format(time.RFC3339))
				}
			}
		})
	}
}
