package inbox_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/inbox"
	"github.com/tggo/go-entitlements/inbox/memory"
	mocksgoogle "github.com/tggo/go-entitlements/internal/mocks/google"
	"github.com/tggo/go-entitlements/internal/testutil"
	storemem "github.com/tggo/go-entitlements/store/memory"
)

// FuzzProcessStored saves arbitrary bodies as Apple or Google items and
// processes them. Whatever the body, it must not panic, and the result must
// be coherent: a nil error means a final status, an error means StatusFailed,
// and the stored item agrees with what was returned.
func FuzzProcessStored(f *testing.F) {
	ca := testutil.NewAppleCA(f)
	v, err := apple.NewVerifier("", apple.WithRootPool(ca.Pool), apple.WithInsecureAnyBundle())
	if err != nil {
		f.Fatal(err)
	}
	var sub google.SubscriptionPurchaseV2
	if err = json.Unmarshal(testutil.GoogleFixture(f, "sub_state_active.json"), &sub); err != nil {
		f.Fatal(err)
	}

	for _, name := range []string{"subscribed", "did_renew", "refund", "refund_reversed", "test", "one_time_charge", "renewal_extension_summary"} {
		f.Add(uint8(0), testutil.LoadAppleFixture(f, name).SignBody(f, ca))
	}
	for _, name := range []string{"notif_subscription.json", "notif_subscription_upgrade.json", "notif_voided.json", "notif_test.json", "notif_unknown.json", "notif_wrong_package.json"} {
		f.Add(uint8(1), testutil.GooglePushBody(f, "m", testutil.GoogleFixture(f, name)))
	}
	f.Add(uint8(0), []byte(`{"signedPayload":"a.b.c"}`))
	f.Add(uint8(1), []byte(`{"message":{"messageId":"1","data":"e30="}}`))
	f.Add(uint8(2), []byte(`not json`))

	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		if len(body) == 0 || len(body) > 1<<16 {
			return
		}
		src := []entitlements.Source{entitlements.SourceApple, entitlements.SourceGoogle, "other"}[int(which)%3]
		fetcher := mocksgoogle.NewMockSubscriptionsFetcher(t)
		fetcher.EXPECT().GetV2(mock.Anything, mock.Anything, mock.Anything).Return(&sub, nil).Maybe()
		gp, gerr := google.NewParser(fetcher, "com.example.app")
		if gerr != nil {
			t.Fatal(gerr)
		}
		in := memory.New()
		p := inbox.Processors{
			Apple: apple.NewParser(v), Google: gp,
			Service: entitlements.NewService(storemem.New(), entitlements.WithAllowSandbox()),
		}
		ctx := context.Background()
		id, err := in.Save(ctx, inbox.Raw{Source: src, Body: body})
		if err != nil {
			return // an invalid source is rejected at Save
		}
		st, perr := inbox.ProcessStored(ctx, in, id, p)
		if !st.Valid() {
			t.Fatalf("invalid status %q", st)
		}
		if (perr == nil) != st.Final() {
			t.Fatalf("status %s with error %v: a retry is requested exactly for non-final statuses", st, perr)
		}
		got, err := in.Get(ctx, id)
		if err != nil || got.Status != st || got.Attempts != 1 {
			t.Fatalf("stored %+v (%v) disagrees with the result %s", got, err, st)
		}
		// a second run never changes a final item
		if st2, err2 := inbox.ProcessStored(ctx, in, id, p); st.Final() && (err2 != nil || st2 != st) {
			t.Fatalf("re-run of a final item = %s, %v; want %s", st2, err2, st)
		}
	})
}
