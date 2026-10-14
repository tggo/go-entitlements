package serverapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
)

const (
	testBundle = "com.example.app"
	testIssuer = "57246542-96fe-1a63-e053-0824d011072a"
	testKeyID  = "2X9R4HXF34"

	origTx = "2000000000000001"
)

// recorded is one request the fake App Store received.
type recorded struct {
	Method string
	Path   string
	Query  map[string][]string
	Body   []byte
	Auth   string
}

// fakeApple emulates the App Store Server API. Handlers answer from a script
// the test sets; every request is recorded.
type fakeApple struct {
	t      testing.TB
	srv    *httptest.Server
	ca     *testutil.AppleCA
	key    *ecdsa.PrivateKey
	keyPEM []byte

	mu      sync.Mutex
	reqs    []recorded
	handler func(w http.ResponseWriter, r recorded)
}

func newFakeApple(t testing.TB) *fakeApple {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	f := &fakeApple{
		t: t, ca: testutil.NewAppleCA(t), key: key,
		keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.Query(), Body: body, Auth: r.Header.Get("Authorization")}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		h := f.handler
		f.mu.Unlock()
		if h == nil {
			http.Error(w, "no handler", http.StatusInternalServerError)
			return
		}
		h(w, rec)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeApple) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeApple) respond(h func(w http.ResponseWriter, r recorded)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// script answers the n-th request with the n-th handler (the last repeats).
func (f *fakeApple) script(steps ...func(w http.ResponseWriter, r recorded)) {
	var n int
	var mu sync.Mutex
	f.respond(func(w http.ResponseWriter, r recorded) {
		mu.Lock()
		i := min(n, len(steps)-1)
		n++
		mu.Unlock()
		steps[i](w, r)
	})
}

func (f *fakeApple) config(env entitlements.Environment) Config {
	return Config{IssuerID: testIssuer, KeyID: testKeyID, PrivateKeyPEM: f.keyPEM, BundleID: testBundle, Environment: env}
}

// verifier trusts the fake's CA and pins the bundle and environment.
func (f *fakeApple) verifier(env entitlements.Environment) *apple.Verifier {
	v, err := apple.NewVerifier(testBundle, apple.WithRootPool(f.ca.Pool), apple.WithEnvironment(env))
	require.NoError(f.t, err)
	return v
}

// client returns a sandbox Client talking to the fake. Sleeps are recorded,
// not slept.
func (f *fakeApple) client(opts ...Option) (*Client, *sleeps) {
	return f.clientEnv(entitlements.EnvironmentSandbox, opts...)
}

func (f *fakeApple) clientEnv(env entitlements.Environment, opts ...Option) (*Client, *sleeps) {
	f.t.Helper()
	all := append([]Option{WithBaseURL(f.srv.URL), WithVerifier(f.verifier(env))}, opts...)
	c, err := New(f.config(env), all...)
	require.NoError(f.t, err)
	s := &sleeps{}
	c.sleep = s.sleep
	return c, s
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(v any) func(http.ResponseWriter, recorded) {
	return func(w http.ResponseWriter, _ recorded) { writeJSON(w, http.StatusOK, v) }
}

// apiErr answers with Apple's error body.
func apiErr(status int, code int64, msg string, hdr ...string) func(http.ResponseWriter, recorded) {
	return func(w http.ResponseWriter, _ recorded) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		writeJSON(w, status, map[string]any{"errorCode": code, "errorMessage": msg})
	}
}

// statusFixture is testdata/serverapi/status_*.json.
type statusFixture struct {
	Status      int            `json:"status"`
	Transaction map[string]any `json:"transaction"`
	Renewal     map[string]any `json:"renewal"`
}

func loadStatus(t testing.TB, name string) statusFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "serverapi", name+".json"))
	require.NoError(t, err)
	var f statusFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

// wireItem signs the fixture and returns the lastTransactionsItem JSON.
func (f statusFixture) wireItem(t testing.TB, ca *testutil.AppleCA) map[string]any {
	t.Helper()
	item := map[string]any{
		"originalTransactionId": f.Transaction["originalTransactionId"],
		"status":                f.Status,
		"signedTransactionInfo": ca.Sign(t, f.Transaction),
	}
	if f.Renewal != nil {
		item["signedRenewalInfo"] = ca.Sign(t, f.Renewal)
	}
	return item
}

// with returns a copy of the fixture with transaction fields overridden.
func (f statusFixture) with(tx map[string]any) statusFixture {
	g := f
	g.Transaction = maps.Clone(f.Transaction)
	maps.Copy(g.Transaction, tx)
	return g
}

func statusResponse(env string, groups ...[]map[string]any) map[string]any {
	data := make([]map[string]any, len(groups))
	for i, items := range groups {
		data[i] = map[string]any{"subscriptionGroupIdentifier": "21000000", "lastTransactions": items}
	}
	return map[string]any{"environment": env, "appAppleId": 1234567890, "bundleId": testBundle, "data": data}
}

// tx builds a transaction payload.
func tx(id string, over map[string]any) map[string]any {
	m := map[string]any{
		"transactionId": id, "originalTransactionId": origTx, "bundleId": testBundle,
		"productId": "com.example.app.monthly", "purchaseDate": 1791000000000, "expiresDate": 1793000000000,
		"type": "Auto-Renewable Subscription", "inAppOwnershipType": "PURCHASED",
		"signedDate": 1791633600000, "environment": "Sandbox",
	}
	maps.Copy(m, over)
	return m
}

// notificationBody signs a notification and returns the JSON a webhook (or a
// history record's signedPayload) carries.
func notificationSigned(t testing.TB, ca *testutil.AppleCA, typ, sub, uuid string, signedMs int64, txn map[string]any, status int) string {
	t.Helper()
	data := map[string]any{"bundleId": testBundle, "environment": "Sandbox"}
	if txn != nil {
		data["signedTransactionInfo"] = ca.Sign(t, txn)
		data["status"] = status
	}
	return ca.Sign(t, map[string]any{
		"notificationType": typ, "subtype": sub, "notificationUUID": uuid,
		"version": "2.0", "signedDate": signedMs, "data": data,
	})
}

func historyRecord(signed string) map[string]any {
	return map[string]any{
		"signedPayload": signed, "firstSendAttemptResult": "SUCCESS",
		"sendAttempts": []map[string]any{{"attemptDate": 1791633600000, "sendAttemptResult": "SUCCESS"}},
	}
}

// decodeJWT splits a compact JWT into its decoded header and claims.
func decodeJWT(t testing.TB, token string) (hdr, claims map[string]any, signingInput string, sig []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	for i, dst := range []*map[string]any{&hdr, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, dst))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	return hdr, claims, parts[0] + "." + parts[1], sig
}

func verifyES256(pub *ecdsa.PublicKey, signingInput string, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(signingInput))
	return ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
