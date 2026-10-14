package apple_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func TestNewVerifier_BundleRequired(t *testing.T) {
	_, err := apple.NewVerifier("")
	require.Error(t, err)

	v, err := apple.NewVerifier("", apple.WithInsecureAnyBundle())
	require.NoError(t, err)
	assert.NotNil(t, v)

	_, err = apple.NewVerifier("com.example.app")
	require.NoError(t, err)
}

func TestNilVerifierParserFails(t *testing.T) {
	_, err := apple.NewParser(nil).Parse([]byte(`{"signedPayload":"a.b.c"}`))
	require.Error(t, err)
}

func TestInsecureAnyBundle_AcceptsForeignApp(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	body := appleBodyMut(t, ca, "subscribed", func(f *testutil.AppleFixture) {
		dataOf(f)["bundleId"] = "com.foreign"
		f.Transaction["bundleId"] = "com.foreign"
	})
	v, err := apple.NewVerifier("", apple.WithRootPool(ca.Pool), apple.WithInsecureAnyBundle())
	require.NoError(t, err)
	_, err = apple.NewParser(v).Parse(body)
	require.NoError(t, err)

	_, err = parserFor(ca).Parse(body) // pinned to com.example.app
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func appleBodyMut(t *testing.T, ca *testutil.AppleCA, name string, mut func(f *testutil.AppleFixture)) []byte {
	t.Helper()
	f := testutil.LoadAppleFixture(t, name)
	mut(&f)
	return f.SignBody(t, ca)
}

func TestNestedIdentityChecks(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	tests := []struct {
		name   string
		opts   []apple.Option
		mut    func(f *testutil.AppleFixture)
		wantOK bool
	}{
		{"control", nil, func(*testutil.AppleFixture) {}, true},
		{"nested transaction bundle mismatch", nil, func(f *testutil.AppleFixture) { f.Transaction["bundleId"] = "com.other" }, false},
		{"nested transaction bundle empty", nil, func(f *testutil.AppleFixture) { delete(f.Transaction, "bundleId") }, false},
		{"nested renewal environment differs from outer", nil, func(f *testutil.AppleFixture) { f.Renewal["environment"] = "Sandbox" }, false},
		{"nested transaction sandbox inside production", nil, func(f *testutil.AppleFixture) { f.Transaction["environment"] = "Sandbox" }, false},
		{"outer bundle mismatch", nil, func(f *testutil.AppleFixture) {
			dataOf(f)["bundleId"] = "com.other"
		}, false},
		{"outer sandbox with production pin", []apple.Option{apple.WithEnvironment("Production")}, func(f *testutil.AppleFixture) {
			dataOf(f)["environment"] = "Sandbox"
			f.Transaction["environment"] = "Sandbox"
			f.Renewal["environment"] = "Sandbox"
		}, false},
		{"sandbox accepted without pin", nil, func(f *testutil.AppleFixture) {
			dataOf(f)["environment"] = "Sandbox"
			f.Transaction["environment"] = "Sandbox"
			f.Renewal["environment"] = "Sandbox"
		}, true},
		{"production pin, production payload", []apple.Option{apple.WithEnvironment("Production")}, func(*testutil.AppleFixture) {}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := appleBodyMut(t, ca, "subscribed", tc.mut)
			_, err := parserFor(ca, tc.opts...).Parse(body)
			if tc.wantOK {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
		})
	}
}

func TestEventCarriesEnvironment(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	ev, err := parserFor(ca).Parse(appleBodyMut(t, ca, "subscribed", func(*testutil.AppleFixture) {}))
	require.NoError(t, err)
	assert.Equal(t, "Production", ev.Environment)
}

// Notifications without "data" carry the bundle in summary / appData /
// externalPurchaseToken; it must be checked there too.
func TestBundleCheckedOnDatalessNotifications(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	for _, key := range []string{"summary", "appData", "externalPurchaseToken"} {
		for _, bundle := range []string{"com.example.app", "com.other"} {
			t.Run(key+"/"+bundle, func(t *testing.T) {
				n := map[string]any{"notificationType": "RENEWAL_EXTENSION", "subtype": "SUMMARY", "notificationUUID": "u", "signedDate": 1,
					key: map[string]any{"bundleId": bundle, "environment": "Production"}}
				b, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, n)})
				_, err := mustV(t, ca).VerifyNotification(b)
				if bundle == "com.other" {
					assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
				} else {
					assert.NoError(t, err)
				}
			})
		}
	}
	t.Run("no bundle source at all", func(t *testing.T) {
		n := map[string]any{"notificationType": "RESCIND_CONSENT", "notificationUUID": "u", "signedDate": 1}
		b, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, n)})
		_, err := mustV(t, ca).VerifyNotification(b)
		assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	})
}

func mustV(t *testing.T, ca *testutil.AppleCA, opts ...apple.Option) *apple.Verifier {
	t.Helper()
	v, err := apple.NewVerifier("com.example.app", append([]apple.Option{apple.WithRootPool(ca.Pool)}, opts...)...)
	require.NoError(t, err)
	return v
}

func TestEventSandboxFlag(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	for env, want := range map[string]bool{"Production": false, "Sandbox": true} {
		ev, err := parserFor(ca).Parse(appleBodyMut(t, ca, "subscribed", func(f *testutil.AppleFixture) {
			dataOf(f)["environment"] = env
			f.Transaction["environment"] = env
			f.Renewal["environment"] = env
		}))
		require.NoError(t, err)
		assert.Equal(t, want, ev.Sandbox, env)
	}
}

func dataOf(f *testutil.AppleFixture) map[string]any {
	d, ok := f.Notification["data"].(map[string]any)
	if !ok {
		panic("fixture has no data object")
	}
	return d
}
