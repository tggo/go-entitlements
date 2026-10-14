package apple_test

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

const token = "7e3fb20b-4cdb-47cc-936d-99d65f608138"

func parserFor(ca *testutil.AppleCA, opts ...apple.Option) *apple.Parser {
	v, err := apple.NewVerifier("com.example.app", append([]apple.Option{apple.WithRootPool(ca.Pool)}, opts...)...)
	if err != nil {
		panic(err)
	}
	return apple.NewParser(v)
}

func defaultRootParser() *apple.Parser {
	v, err := apple.NewVerifier("com.example.app")
	if err != nil {
		panic(err)
	}
	return apple.NewParser(v)
}

func TestParser_MapsEveryType(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := parserFor(ca)
	ms := time.UnixMilli

	tests := []struct {
		fixture string
		state   entitlements.State
		expires time.Time
	}{
		{"subscribed", entitlements.StateActive, ms(1760500000000).UTC()},
		{"did_renew", entitlements.StateActive, ms(1760500000000).UTC()},
		{"offer_redeemed", entitlements.StateActive, ms(1760500000000).UTC()},
		{"did_change_renewal_pref", entitlements.StateActive, ms(1760500000000).UTC()},
		{"did_fail_to_renew_grace", entitlements.StateGrace, ms(1760900000000).UTC()}, // extended to grace expiry
		{"did_fail_to_renew", entitlements.StateBillingRetry, ms(1759000000000).UTC()},
		{"grace_period_expired", entitlements.StateBillingRetry, ms(1759000000000).UTC()},
		{"expired", entitlements.StateExpired, ms(1759000000000).UTC()},
		{"refund", entitlements.StateRefunded, ms(1760500000000).UTC()},
		{"revoke", entitlements.StateRevoked, ms(1760500000000).UTC()},
		{"renewal_status_off", entitlements.StateActive, ms(1760500000000).UTC()},
		{"renewal_status_off_lapsed", entitlements.StateExpired, ms(1759000000000).UTC()},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			body := testutil.LoadAppleFixture(t, tc.fixture).SignBody(t, ca)
			ev, err := p.Parse(body)
			require.NoError(t, err)
			assert.Equal(t, "uuid-"+tc.fixture, ev.ID)
			assert.Equal(t, entitlements.SourceApple, ev.Source)
			assert.Equal(t, token, ev.UserID)
			assert.Equal(t, "2000000000001", ev.OriginalTxID)
			assert.Equal(t, "com.example.pro.monthly", ev.ProductID)
			assert.Equal(t, tc.state, ev.State)
			assert.True(t, tc.expires.Equal(ev.ExpiresAt), "expires %v want %v", ev.ExpiresAt, tc.expires)
			assert.True(t, ms(1760000000000).Equal(ev.OccurredAt))
			assert.Equal(t, body, ev.Raw)
		})
	}
}

func TestParser_GraceKeepsAccess(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	ev, err := parserFor(ca).Parse(testutil.LoadAppleFixture(t, "did_fail_to_renew_grace").SignBody(t, ca))
	require.NoError(t, err)
	e := entitlements.Entitlement{State: ev.State, ExpiresAt: ev.ExpiresAt}
	assert.True(t, e.Active(time.UnixMilli(1760700000000)), "within grace window")
}

func TestParser_IgnoredAndUnsupported(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := parserFor(ca)

	_, err := p.Parse(testutil.LoadAppleFixture(t, "test").SignBody(t, ca))
	assert.ErrorIs(t, err, entitlements.ErrIgnored)

	_, err = p.Parse(testutil.LoadAppleFixture(t, "price_increase").SignBody(t, ca))
	assert.ErrorIs(t, err, entitlements.ErrIgnored)
}

func TestParser_MissingTransactionData(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := parserFor(ca)

	f := testutil.LoadAppleFixture(t, "subscribed")
	f.Transaction = nil
	_, err := p.Parse(f.SignBody(t, ca))
	require.Error(t, err)
	assert.NotErrorIs(t, err, entitlements.ErrInvalidSignature)

}

func TestParser_NoAccountToken(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	ev, err := parserFor(ca).Parse(testutil.LoadAppleFixture(t, "subscribed_no_account_token").SignBody(t, ca))
	require.NoError(t, err)
	assert.Empty(t, ev.UserID)
	assert.Equal(t, "2000000000001", ev.OriginalTxID)
	assert.Equal(t, entitlements.StateActive, ev.State)
}

func TestParser_OneTimeChargeIgnored(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	_, err := parserFor(ca).Parse(testutil.LoadAppleFixture(t, "one_time_charge").SignBody(t, ca))
	require.ErrorIs(t, err, entitlements.ErrIgnored)
	assert.Contains(t, err.Error(), "Non-Consumable")
}

func TestToEvent_Nil(t *testing.T) {
	_, err := apple.ToEvent(nil)
	assert.Error(t, err)
}

func TestVerifier_Rejections(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	good := testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca)
	var rb apple.ResponseBodyV2
	require.NoError(t, json.Unmarshal(good, &rb))
	parts := strings.Split(rb.SignedPayload, ".")
	body := func(jws string) []byte {
		b, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: jws})
		return b
	}

	otherCA := testutil.NewAppleCA(t)
	forged := ca.SignWithKey(t, testutil.NewAppleKey(t), map[string]any{"notificationType": "SUBSCRIBED"})

	tamperedPayload := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"notificationType":"REFUND"}`)) + "." + parts[2]

	badAlg := func() string {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		return h + "." + parts[1] + "." + parts[2]
	}()
	noX5C := func() string {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256"}`))
		return h + "." + parts[1] + "." + parts[2]
	}()

	tests := []struct {
		name   string
		parser *apple.Parser
		body   []byte
	}{
		{"tampered payload", parserFor(ca), body(tamperedPayload)},
		{"signature by other key", parserFor(ca), body(forged)},
		{"chain to wrong custom root", parserFor(otherCA), good},
		{"chain to wrong root (default Apple root)", defaultRootParser(), good},
		{"leaf lacks OID", parserFor(testutil.NewAppleCA(t, testutil.AppleCAOptions{OmitLeafOID: true})), nil},
		{"intermediate lacks OID", parserFor(testutil.NewAppleCA(t, testutil.AppleCAOptions{OmitIntermediateOID: true})), nil},
		{"expired leaf", parserFor(testutil.NewAppleCA(t, testutil.AppleCAOptions{ExpiredLeaf: true})), nil},
		{"two segments", parserFor(ca), body("a.b")},
		{"bad header base64", parserFor(ca), body("!!!." + parts[1] + "." + parts[2])},
		{"bad header json", parserFor(ca), body(base64.RawURLEncoding.EncodeToString([]byte("{")) + "." + parts[1] + "." + parts[2])},
		{"alg none", parserFor(ca), body(badAlg)},
		{"no x5c", parserFor(ca), body(noX5C)},
		{"bad signature length", parserFor(ca), body(parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("short")))},
		{"bad signature base64", parserFor(ca), body(parts[0] + "." + parts[1] + ".***")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.body
			if b == nil { // chain-variant cases: sign with the broken CA itself
				ca := brokenCA(t, tc.name)
				tc.parser = parserFor(ca)
				b = testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca)
			}
			_, err := tc.parser.Parse(b)
			require.Error(t, err)
			assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
		})
	}
}

func brokenCA(t *testing.T, name string) *testutil.AppleCA {
	t.Helper()
	switch name {
	case "leaf lacks OID":
		return testutil.NewAppleCA(t, testutil.AppleCAOptions{OmitLeafOID: true})
	case "intermediate lacks OID":
		return testutil.NewAppleCA(t, testutil.AppleCAOptions{OmitIntermediateOID: true})
	default:
		return testutil.NewAppleCA(t, testutil.AppleCAOptions{ExpiredLeaf: true})
	}
}

func TestVerifier_TimeFn(t *testing.T) {
	ca := testutil.NewAppleCA(t, testutil.AppleCAOptions{ExpiredLeaf: true})
	body := testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca)
	// Leaf was valid from now-48h to now-24h; pin the clock into that window.
	at := time.Now().Add(-36 * time.Hour)
	_, err := parserFor(ca, apple.WithTimeFn(func() time.Time { return at })).Parse(body)
	require.NoError(t, err)
}

func TestVerifier_NotificationBadBody(t *testing.T) {
	p := defaultRootParser()
	for name, b := range map[string]string{"not json": "nope", "empty payload": `{"signedPayload":""}`} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Parse([]byte(b))
			require.Error(t, err)
			assert.NotErrorIs(t, err, entitlements.ErrInvalidSignature)
		})
	}
}

func TestVerifier_BundleAndEnvironment(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	body := testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca)

	_, err := parserFor(ca, apple.WithEnvironment(entitlements.EnvironmentProduction)).Parse(body)
	require.NoError(t, err)

	v, err := apple.NewVerifier("com.other.app", apple.WithRootPool(ca.Pool))
	require.NoError(t, err)
	_, err = apple.NewParser(v).Parse(body)
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)

	_, err = parserFor(ca, apple.WithEnvironment(entitlements.EnvironmentSandbox)).Parse(body)
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestVerifier_NestedJWSInvalid(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	f := testutil.LoadAppleFixture(t, "subscribed")
	forged := ca.SignWithKey(t, testutil.NewAppleKey(t), f.Transaction)

	for _, field := range []string{"signedTransactionInfo", "signedRenewalInfo"} {
		t.Run(field, func(t *testing.T) {
			n := map[string]any{}
			for k, v := range f.Notification {
				n[k] = v
			}
			d := map[string]any{"bundleId": "com.example.app", "environment": "Production",
				"signedTransactionInfo": ca.Sign(t, f.Transaction), "signedRenewalInfo": ca.Sign(t, f.Renewal)}
			d[field] = forged
			n["data"] = d
			b, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, n)})
			_, err := parserFor(ca).Parse(b)
			assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
		})
	}
}

func TestVerify_BadPayloadJSON(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	jws := ca.Sign(t, []int{1, 2})
	var out apple.NotificationPayload
	err := mustV(t, ca).Verify(jws, &out)
	require.Error(t, err)
	assert.NotErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestVerify_WrongKeyType(t *testing.T) {
	// Leaf with a bad x5c entry.
	ca := testutil.NewAppleCA(t)
	jws := ca.Sign(t, map[string]string{})
	parts := strings.Split(jws, ".")
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","x5c":["!!","AAAA"]}`))
	v := mustV(t, ca)
	assert.ErrorIs(t, v.Verify(h+"."+parts[1]+"."+parts[2], &struct{}{}), entitlements.ErrInvalidSignature)
	h = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","x5c":["AAAA","AAAA"]}`))
	assert.ErrorIs(t, v.Verify(h+"."+parts[1]+"."+parts[2], &struct{}{}), entitlements.ErrInvalidSignature)
}

func TestEmbeddedRootCA(t *testing.T) {
	pool := apple.RootCAG3Pool()
	require.NotNil(t, pool)
	der, err := os.ReadFile("AppleRootCA-G3.cer")
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	assert.Equal(t, "Apple Root CA - G3", cert.Subject.CommonName)
	want := x509.NewCertPool()
	want.AddCert(cert)
	assert.True(t, pool.Equal(want))
}

func TestRootCAG3Pool_ReturnsIndependentCopies(t *testing.T) {
	a := apple.RootCAG3Pool()
	a.AddCert(testutil.NewAppleCA(t).Root) // caller mutates its copy
	b := apple.RootCAG3Pool()
	assert.False(t, a.Equal(b))
	assert.True(t, b.Equal(apple.RootCAG3Pool()), "the shared pool is untouched")
}

func TestNilOptionsKeepDefaults(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	v, err := apple.NewVerifier("com.example.app", nil, apple.WithTimeFn(nil), apple.WithRootPool(nil))
	require.NoError(t, err)
	// default roots are Apple's, so a test-CA chain is rejected rather than panicking
	_, err = apple.NewParser(v).Parse(testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca))
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestVerify_X5CCountCapped(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	der := ca.Leaf.Raw
	jws := testutil.SignChain(t, ca.LeafKey, [][]byte{der, der, der, der, der, der}, map[string]string{})
	err := mustV(t, ca).Verify(jws, &struct{}{})
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "at most")
}

func base64RawURL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
