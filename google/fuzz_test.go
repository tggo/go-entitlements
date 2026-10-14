package google_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/mocks"
	"github.com/tggo/go-entitlements/internal/testutil"
)

// FuzzGoogleParse feeds arbitrary RTDN payloads, wrapped in a valid Pub/Sub
// envelope, and arbitrary subscriptionsv2 documents. Parse may return any
// error, but must never panic.
func FuzzGoogleParse(f *testing.F) {
	pairs := [][2]string{
		{"notif_subscription.json", "sub_state_active.json"},
		{"notif_subscription_upgrade.json", "sub_linked_upgrade.json"},
		{"notif_subscription.json", "sub_state_in_grace_period.json"},
		{"notif_subscription.json", "sub_state_canceled.json"},
		{"notif_subscription.json", "sub_state_on_hold.json"},
		{"notif_subscription.json", "sub_state_paused.json"},
		{"notif_subscription.json", "sub_state_expired.json"},
		{"notif_subscription.json", "sub_state_unknown.json"},
		{"notif_voided.json", "sub_state_active.json"},
		{"notif_test.json", "sub_state_active.json"},
		{"notif_unknown.json", "sub_state_active.json"},
		{"notif_bad_json.json", "sub_state_active.json"},
	}
	for _, p := range pairs {
		f.Add(testutil.GoogleFixture(f, p[0]), testutil.GoogleFixture(f, p[1]))
	}
	f.Add([]byte(`{"message":{"messageId":"1","data":"e30="}}`), []byte(`{}`))

	f.Fuzz(func(t *testing.T, notification, subscription []byte) {
		var purchase google.SubscriptionPurchaseV2
		if json.Unmarshal(subscription, &purchase) != nil {
			return
		}
		fetcher := mocks.NewMockSubscriptionsFetcher(t)
		fetcher.EXPECT().GetV2(mock.Anything, mock.Anything, mock.Anything).Return(&purchase, nil).Maybe()
		parser, err := google.NewParser(fetcher)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		_, _ = parser.Parse(ctx, notification)                                   // raw body
		_, _ = parser.Parse(ctx, testutil.GooglePushBody(t, "m1", notification)) // wrapped as the RTDN payload
	})
}
