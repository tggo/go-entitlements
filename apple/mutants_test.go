package apple_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func chainHeader(ca *testutil.AppleCA, alg string) map[string]any {
	var x5c []string
	for _, der := range ca.Chain() {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(der))
	}
	return map[string]any{"alg": alg, "x5c": x5c}
}

func TestVerify_RejectsOtherAlgorithms(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	v := mustV(t, ca)
	for _, alg := range []string{"none", "HS256", "ES384", "RS256", ""} {
		// the signature is a genuine ES256 one; only the declared alg is wrong
		jws := testutil.SignRawHeader(t, ca.LeafKey, chainHeader(ca, alg), []byte(`{}`))
		err := v.Verify(jws, &struct{}{})
		require.ErrorIs(t, err, entitlements.ErrInvalidSignature, alg)
		assert.Contains(t, err.Error(), "unsupported alg", alg)
	}
	jws := testutil.SignRawHeader(t, ca.LeafKey, chainHeader(ca, "ES256"), []byte(`{}`))
	require.NoError(t, v.Verify(jws, &struct{}{}))
}

func TestVerify_X5CCountBoundaries(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	v := mustV(t, ca)
	chain := ca.Chain()
	sign := func(n int) string {
		certs := make([][]byte, n)
		for i := range certs {
			certs[i] = chain[min(i, len(chain)-1)]
		}
		return testutil.SignChain(t, ca.LeafKey, certs, map[string]string{})
	}
	for _, n := range []int{0, 1} {
		err := v.Verify(sign(n), &struct{}{})
		require.ErrorIs(t, err, entitlements.ErrInvalidSignature, n)
		assert.Contains(t, err.Error(), "x5c must contain", n)
	}
	require.NoError(t, v.Verify(sign(2), &struct{}{}), "leaf and intermediate are enough when the root is pinned")
	require.NoError(t, v.Verify(sign(5), &struct{}{}), "exactly the cap")
	err := v.Verify(sign(6), &struct{}{})
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "at most")
}

func TestVerify_ChainWithTwoIntermediates(t *testing.T) {
	ca := testutil.NewAppleCA(t, testutil.AppleCAOptions{ExtraIntermediate: true})
	require.NotNil(t, ca.Upper)
	require.Len(t, ca.Chain(), 4)
	require.NoError(t, mustV(t, ca).Verify(ca.Sign(t, map[string]string{}), &struct{}{}))
}

func TestVerify_FourSegmentTokenIsMalformed(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	good := ca.Sign(t, map[string]string{})
	err := mustV(t, ca).Verify(good+".extra", &struct{}{})
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "want 3 segments")
}

func notificationBody(t *testing.T, ca *testutil.AppleCA, mut func(f *testutil.AppleFixture)) []byte {
	t.Helper()
	f := testutil.LoadAppleFixture(t, "subscribed")
	mut(&f)
	return f.SignBody(t, ca)
}

func TestVerifyNotification_EnvironmentPinFailsAtTheOuterCheck(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	sandbox := notificationBody(t, ca, func(f *testutil.AppleFixture) {
		f.Data()["environment"] = "Sandbox"
		f.Transaction["environment"] = "Sandbox"
		f.Renewal["environment"] = "Sandbox"
	})
	_, err := mustV(t, ca, apple.WithEnvironment(entitlements.EnvironmentProduction)).VerifyNotification(sandbox)
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), `environment "Sandbox", want "Production"`)
	assert.NotContains(t, err.Error(), "signedTransactionInfo", "must fail before the nested payloads are looked at")

	_, err = mustV(t, ca, apple.WithEnvironment(entitlements.EnvironmentSandbox)).VerifyNotification(sandbox)
	require.NoError(t, err)
}

func TestVerifyNotification_NestedEnvironmentMustMatchTheOuterOne(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	for name, tc := range map[string]struct {
		mut func(f *testutil.AppleFixture)
		ok  bool
	}{
		"nested empty, outer set": {func(f *testutil.AppleFixture) { delete(f.Transaction, "environment"); delete(f.Renewal, "environment") }, true},
		"outer empty, nested set": {func(f *testutil.AppleFixture) { delete(f.Data(), "environment") }, true},
		"both set and equal":      {func(*testutil.AppleFixture) {}, true},
		"both set and different":  {func(f *testutil.AppleFixture) { f.Transaction["environment"] = "Sandbox" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mustV(t, ca).VerifyNotification(notificationBody(t, ca, tc.mut))
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
		})
	}
}

// data is the identity source when a payload carries several; summary must not override it.
func TestVerifyNotification_DataWinsOverSummary(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	n := map[string]any{
		"notificationType": "TEST", "notificationUUID": "u", "signedDate": 1,
		"data":    map[string]any{"bundleId": "com.example.app", "environment": "Production"},
		"summary": map[string]any{"bundleId": "com.other", "environment": "Sandbox"},
	}
	b, err := json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, n)})
	require.NoError(t, err)
	_, err = mustV(t, ca, apple.WithEnvironment(entitlements.EnvironmentProduction)).VerifyNotification(b)
	require.NoError(t, err)

	// and the other way round: a bad data bundle is not rescued by a good summary
	n["data"] = map[string]any{"bundleId": "com.other", "environment": "Production"}
	n["summary"] = map[string]any{"bundleId": "com.example.app", "environment": "Production"}
	b, _ = json.Marshal(apple.ResponseBodyV2{SignedPayload: ca.Sign(t, n)})
	_, err = mustV(t, ca).VerifyNotification(b)
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
}

func TestVerify_ErrorMessagesAreSpecific(t *testing.T) {
	_, err := apple.NewParser(nil).Parse([]byte(`{"signedPayload":"a.b.c"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no verifier")

	_, err = apple.ToEvent(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil notification")
	assert.False(t, strings.Contains(err.Error(), "signature"))
}

// With an environment pinned, a nested payload that does not say its
// environment is not trusted to be the pinned one.
func TestVerifyNotification_PinnedEnvironmentRejectsNestedWithoutEnvironment(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	for _, nested := range []string{"Transaction", "Renewal"} {
		body := notificationBody(t, ca, func(f *testutil.AppleFixture) {
			if nested == "Transaction" {
				delete(f.Transaction, "environment")
			} else {
				delete(f.Renewal, "environment")
			}
		})
		_, err := mustV(t, ca, apple.WithEnvironment(entitlements.EnvironmentProduction)).VerifyNotification(body)
		require.ErrorIs(t, err, entitlements.ErrInvalidSignature, nested)
		assert.Contains(t, err.Error(), "environment", nested)

		_, err = mustV(t, ca).VerifyNotification(body) // no pin: fine
		require.NoError(t, err, nested)
	}
}

func TestVerify_RejectsALeafThatIsNotP256(t *testing.T) {
	ca := testutil.NewAppleCA(t, testutil.AppleCAOptions{LeafP384: true})
	jws := ca.SignWithKey(t, testutil.NewAppleKey(t), map[string]string{})
	err := mustV(t, ca).Verify(jws, &struct{}{})
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.Contains(t, err.Error(), "P-256")
}
