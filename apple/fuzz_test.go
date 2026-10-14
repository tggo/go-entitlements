package apple_test

import (
	"encoding/json"
	"testing"

	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

// FuzzAppleParse feeds arbitrary notification, transaction and renewal
// payloads — signed by a trusted test CA so they get past verification and
// reach the decoder and mapper — plus the raw notification as a request body.
// Parse may return any error, but must never panic.
func FuzzAppleParse(f *testing.F) {
	ca := testutil.NewAppleCA(f)
	v, err := apple.NewVerifier("", apple.WithRootPool(ca.Pool), apple.WithInsecureAnyBundle())
	if err != nil {
		f.Fatal(err)
	}
	p := apple.NewParser(v)

	for _, name := range []string{
		"subscribed", "did_renew", "did_fail_to_renew_grace", "expired", "refund", "refund_reversed",
		"renewal_extended", "renewal_status_off", "test", "one_time_charge", "price_increase",
		"renewal_extension_summary", "subscribed_no_account_token",
	} {
		fx := testutil.LoadAppleFixture(f, name)
		n, _ := json.Marshal(fx.Notification)
		tx, _ := json.Marshal(fx.Transaction)
		ri, _ := json.Marshal(fx.Renewal)
		f.Add(n, tx, ri)
	}
	f.Add([]byte(`{"signedPayload":"a.b.c"}`), []byte(`[]`), []byte(`null`))

	f.Fuzz(func(t *testing.T, notification, tx, renewal []byte) {
		_, _ = p.Parse(notification) // raw body path

		payload := notification
		var n map[string]any
		if json.Unmarshal(notification, &n) == nil && n != nil {
			data, _ := n["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			if len(tx) > 0 {
				data["signedTransactionInfo"] = ca.SignRaw(t, tx)
			}
			if len(renewal) > 0 {
				data["signedRenewalInfo"] = ca.SignRaw(t, renewal)
			}
			n["data"] = data
			payload, _ = json.Marshal(n)
		}
		body, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.SignRaw(t, payload)})
		_, _ = p.Parse(body)
	})
}
