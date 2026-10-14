package apple_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

// Fixtures from Apple's own client libraries (MIT), see testdata/prior-art/*/SOURCE.md.
const (
	pyDir   = "../testdata/prior-art/app-store-server-library-python"
	javaDir = "../testdata/prior-art/app-store-server-library-java/certs"
)

func readFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	require.NoError(t, err)
	return b
}

func pemDER(t *testing.T, name string) []byte {
	t.Helper()
	blk, _ := pem.Decode(readFile(t, javaDir, name))
	require.NotNil(t, blk, name)
	return blk.Bytes
}

func appleLibVerifier(t *testing.T, bundle string) *apple.Verifier {
	t.Helper()
	ca, err := x509.ParseCertificate(readFile(t, pyDir, "certs", "testCA.der"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	v, err := apple.NewVerifier(bundle, apple.WithRootPool(pool),
		apple.WithTimeFn(func() time.Time { return time.Date(2023, 4, 12, 0, 0, 0, 0, time.UTC) }))
	require.NoError(t, err)
	return v
}

func mockJWS(t *testing.T, name string) string {
	return strings.TrimSpace(string(readFile(t, pyDir, "mock_signed_data", name)))
}

func TestPriorArt_AppleLibraryJWSCompatibility(t *testing.T) {
	v := appleLibVerifier(t, "com.example")
	var n apple.NotificationPayload
	require.NoError(t, v.Verify(mockJWS(t, "testNotification"), &n))
	assert.Equal(t, "TEST", n.NotificationType)
	var tx apple.JWSTransactionDecodedPayload
	require.NoError(t, v.Verify(mockJWS(t, "transactionInfo"), &tx))
	assert.Equal(t, "com.example", tx.BundleID)
	var ri apple.JWSRenewalInfoDecodedPayload
	require.NoError(t, v.Verify(mockJWS(t, "renewalInfo"), &ri))

	body, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: mockJWS(t, "testNotification")})
	_, err := apple.NewParser(v).Parse(body)
	assert.ErrorIs(t, err, entitlements.ErrIgnored)

	body, _ = json.Marshal(apple.ResponseBodyV2{SignedPayload: mockJWS(t, "wrongBundleId")})
	_, err = apple.NewParser(v).Parse(body)
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestPriorArt_MalformedAppleSamples(t *testing.T) {
	v := appleLibVerifier(t, "com.example")
	var out map[string]any
	err := v.Verify(mockJWS(t, "missingX5CHeaderClaim"), &out)
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "x5c must contain")

	err = v.Verify(mockJWS(t, "legacyTransaction"), &out)
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "want 3 segments")
}

func TestPriorArt_WrongMarkerOID(t *testing.T) {
	keyPEM, _ := pem.Decode(readFile(t, javaDir, "testLeaf.key"))
	require.NotNil(t, keyPEM)
	var key *ecdsa.PrivateKey
	key, err := x509.ParseECPrivateKey(keyPEM.Bytes)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(pemDER(t, "testCA.pem"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	v, err := apple.NewVerifier("", apple.WithInsecureAnyBundle(), apple.WithRootPool(pool))
	require.NoError(t, err)

	tests := []struct {
		name, leaf, inter, wantErr string
	}{
		{"control", "testLeaf.pem", "testIntermediate.pem", ""},
		{"invalid leaf OID", "testInvalidOIDLeaf.pem", "testIntermediate.pem", "1.2.840.113635.100.6.11.1"},
		{"invalid intermediate OID", "testLeaf.pem", "testInvalidOIDIntermediate.pem", "1.2.840.113635.100.6.2.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jws := testutil.SignChain(t, key, [][]byte{pemDER(t, tc.leaf), pemDER(t, tc.inter)}, map[string]string{"k": "v"})
			var out map[string]string
			err := v.Verify(jws, &out)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Decoded fixtures hold placeholder strings for nested JWS; re-sign with the
// test CA as examples/sign-fixture does.
func TestPriorArt_DatalessAndUnmappedNotifications(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	v, err := apple.NewVerifier("com.example", apple.WithRootPool(ca.Pool))
	require.NoError(t, err)
	p := apple.NewParser(v)

	load := func(name string) map[string]any {
		var m map[string]any
		require.NoError(t, json.Unmarshal(readFile(t, pyDir, "models", name), &m))
		return m
	}
	body := func(m map[string]any) []byte {
		b, _ := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, m)})
		return b
	}

	// RENEWAL_EXTENSION/SUMMARY: ignored (no subscription state), README expected Unsupported before gap 6.
	_, err = p.Parse(body(load("signedSummaryNotification.json")))
	assert.ErrorIs(t, err, entitlements.ErrIgnored)

	for _, name := range []string{"signedRescindConsentNotification.json", "signedExternalPurchaseTokenNotification.json"} {
		_, err = p.Parse(body(load(name)))
		assert.ErrorIs(t, err, entitlements.ErrUnsupported, name)
	}

	cr := load("signedConsumptionRequestNotification.json")
	data := cr["data"].(map[string]any)
	data["signedTransactionInfo"] = ca.Sign(t, map[string]any{"transactionId": "1", "originalTransactionId": "1", "bundleId": "com.example", "productId": "p", "environment": "LocalTesting"})
	data["signedRenewalInfo"] = ca.Sign(t, map[string]any{"environment": "LocalTesting"})
	_, err = p.Parse(body(cr))
	assert.ErrorIs(t, err, entitlements.ErrUnsupported)

	// Placeholder nested strings fail verification rather than being trusted.
	_, err = p.Parse(body(load("signedNotification.json")))
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestGraceWithoutRenewalInfo(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	f := testutil.LoadAppleFixture(t, "did_fail_to_renew_grace")
	f.Renewal = nil
	ev, err := parserFor(ca).Parse(f.SignBody(t, ca))
	require.NoError(t, err)
	// Pinned: without renewal info grace cannot be extended, so ExpiresAt is the
	// (already past) transaction expiry and the entitlement is not Active.
	assert.Equal(t, entitlements.StateGrace, ev.State)
	assert.True(t, time.UnixMilli(1759000000000).Equal(ev.ExpiresAt))
	assert.False(t, entitlements.Entitlement{State: ev.State, ExpiresAt: ev.ExpiresAt}.Active(time.UnixMilli(1760000000000)))
}

func TestGarbageInput(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	p := parserFor(ca)
	enc := func(s string) string { return base64RawURL([]byte(s)) }

	for name, body := range map[string]string{
		"x5c of random strings": `{"signedPayload":"` + enc(`{"alg":"ES256","x5c":["abc","def","ghi"]}`) + `.` + enc(`{}`) + `.` + enc("sig") + `"}`,
		"truncated json":        `{"signedPayload":"abc`,
		"payload is not object": `{"signedPayload":"` + enc(`{"alg":"ES256","x5c":[]}`) + `.e30.AA"}`,
		"empty body":            ``,
		"null":                  `null`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotPanics(t, func() {
				_, err := p.Parse([]byte(body))
				assert.Error(t, err)
			})
		})
	}

	// Empty signedRenewalInfo is simply absent.
	f := testutil.LoadAppleFixture(t, "subscribed")
	f.Renewal = nil
	_, err := p.Parse(f.SignBody(t, ca))
	assert.NoError(t, err)
}
