package serverapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
)

var fixedNow = time.UnixMilli(1791633600000).UTC()

func fixedClock() time.Time { return fixedNow }

func TestJWT_ClaimsAndSignature(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"testNotificationToken": "tok"}))
	c, _ := f.client(WithClock(fixedClock))

	_, err := c.RequestTestNotification(context.Background())
	require.NoError(t, err)

	reqs := f.requests()
	require.Len(t, reqs, 1)
	token, found := strings.CutPrefix(reqs[0].Auth, "Bearer ")
	require.True(t, found, "Authorization must be a bearer token")
	hdr, claims, input, sig := decodeJWT(t, token)

	assert.Equal(t, map[string]any{"alg": "ES256", "kid": testKeyID, "typ": "JWT"}, hdr)
	assert.Equal(t, testIssuer, claims["iss"])
	assert.Equal(t, "appstoreconnect-v1", claims["aud"])
	assert.Equal(t, testBundle, claims["bid"])
	assert.EqualValues(t, fixedNow.Unix(), claims["iat"])
	assert.EqualValues(t, fixedNow.Add(5*time.Minute).Unix(), claims["exp"], "default lifetime is 5 minutes")
	assert.True(t, verifyES256(&f.key.PublicKey, input, sig), "signature must verify with the key's public half")
	assert.Len(t, claims, 5, "no extra claims")
}

func TestJWT_TokenTTL(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"testNotificationToken": "tok"}))

	t.Run("custom lifetime up to an hour", func(t *testing.T) {
		c, _ := f.client(WithClock(fixedClock), WithTokenTTL(time.Hour))
		_, err := c.RequestTestNotification(context.Background())
		require.NoError(t, err)
		reqs := f.requests()
		_, claims, _, _ := decodeJWT(t, strings.TrimPrefix(reqs[len(reqs)-1].Auth, "Bearer "))
		assert.EqualValues(t, fixedNow.Add(time.Hour).Unix(), claims["exp"])
	})
	t.Run("more than an hour is refused", func(t *testing.T) {
		_, err := New(f.config(entitlements.EnvironmentSandbox), WithTokenTTL(time.Hour+time.Second))
		require.ErrorContains(t, err, "exceeds Apple's limit")
	})
}

func TestJWT_FreshTokenPerRequest(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"testNotificationToken": "tok"}))
	c, _ := f.client(WithClock(fixedClock))
	for range 2 {
		_, err := c.RequestTestNotification(context.Background())
		require.NoError(t, err)
	}
	reqs := f.requests()
	assert.NotEqual(t, reqs[0].Auth, reqs[1].Auth, "every request is signed anew")
}

func TestNew_Validation(t *testing.T) {
	f := newFakeApple(t)
	good := f.config(entitlements.EnvironmentProduction)

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	p384DER, err := x509.MarshalPKCS8PrivateKey(p384)
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)
	enc := func(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}) }

	tests := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"no issuer", func(c *Config) { c.IssuerID = "" }, "IssuerID"},
		{"no key id", func(c *Config) { c.KeyID = "" }, "KeyID"},
		{"no bundle", func(c *Config) { c.BundleID = "" }, "BundleID"},
		{"not pem", func(c *Config) { c.PrivateKeyPEM = []byte("nope") }, "not PEM"},
		{"empty key", func(c *Config) { c.PrivateKeyPEM = nil }, "not PEM"},
		{"garbage der", func(c *Config) { c.PrivateKeyPEM = enc([]byte("garbage")) }, "not PKCS#8"},
		{"rsa key", func(c *Config) { c.PrivateKeyPEM = enc(rsaDER) }, "P-256"},
		{"p384 key", func(c *Config) { c.PrivateKeyPEM = enc(p384DER) }, "P-256"},
		{"xcode env", func(c *Config) { c.Environment = entitlements.EnvironmentXcode }, "unsupported environment"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.mod(&cfg)
			_, err := New(cfg)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "PRIVATE KEY")
		})
	}

	t.Run("empty environment means production", func(t *testing.T) {
		cfg := good
		cfg.Environment = ""
		c, err := New(cfg)
		require.NoError(t, err)
		assert.Equal(t, entitlements.EnvironmentProduction, c.Environment())
		assert.Equal(t, ProductionURL, c.base)
		assert.Equal(t, testBundle, c.BundleID())
	})
	t.Run("sandbox uses the sandbox host", func(t *testing.T) {
		cfg := good
		cfg.Environment = entitlements.EnvironmentSandbox
		c, err := New(cfg)
		require.NoError(t, err)
		assert.Equal(t, SandboxURL, c.base)
	})
	t.Run("base URL override drops trailing slash", func(t *testing.T) {
		c, err := New(good, WithBaseURL("http://localhost:1/"))
		require.NoError(t, err)
		assert.Equal(t, "http://localhost:1", c.base)
	})
	t.Run("nil options are ignored", func(t *testing.T) {
		_, err := New(good, nil, WithHTTPClient(nil), WithClock(nil), WithVerifier(nil), WithLogger(nil))
		require.NoError(t, err)
	})
}

// The key and the tokens must not reach logs, fmt output or error strings.
func TestSecretsAreNeverPrinted(t *testing.T) {
	f := newFakeApple(t)
	f.script(apiErr(http.StatusServiceUnavailable, 5000001, "try again"), ok(map[string]any{"testNotificationToken": "tok"}))
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, _ := f.client(WithLogger(log))

	cfg := f.config(entitlements.EnvironmentSandbox)
	for _, s := range []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprintf("%v", c.signer), fmt.Sprintf("%+v", c.signer), fmt.Sprintf("%#v", c.signer),
	} {
		assert.NotContains(t, s, "PRIVATE KEY")
		assert.NotContains(t, s, string(cfg.PrivateKeyPEM))
		assert.Contains(t, s, "redacted")
	}
	slog.New(slog.NewTextHandler(&logs, nil)).Info("config", "cfg", cfg)

	_, err := c.RequestTestNotification(context.Background())
	require.NoError(t, err)

	for _, rec := range f.requests() {
		token := strings.TrimPrefix(rec.Auth, "Bearer ")
		require.NotEmpty(t, token)
		assert.NotContains(t, logs.String(), token, "the JWT must not be logged")
		assert.NotContains(t, logs.String(), strings.Split(token, ".")[2], "nor its signature")
	}
	assert.NotContains(t, logs.String(), "PRIVATE KEY")
	assert.Contains(t, logs.String(), "serverapi: retrying", "the retry itself is logged")
}

func TestParsePrivateKey_ErrorsDoNotEchoInput(t *testing.T) {
	_, err := ParsePrivateKey([]byte("-----BEGIN " + "PRIVATE KEY-----\nc2VjcmV0LXN0dWZm\n-----END " + "PRIVATE KEY-----\n"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "c2VjcmV0")
}

func TestRetry_RateLimitHonoursRetryAfterMillis(t *testing.T) {
	f := newFakeApple(t)
	at := fixedNow.Add(2 * time.Second).UnixMilli()
	f.script(
		apiErr(http.StatusTooManyRequests, 4290000, "Rate limit exceeded.", "Retry-After", strconv.FormatInt(at, 10)),
		ok(map[string]any{"testNotificationToken": "tok"}),
	)
	c, sl := f.client(WithClock(fixedClock))

	token, err := c.RequestTestNotification(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok", token)
	assert.Len(t, f.requests(), 2)
	assert.Equal(t, []time.Duration{2 * time.Second}, sl.all(), "waits until the UNIX-ms timestamp Apple sent")
}

func TestRetry_RateLimitHonoursRetryAfterSeconds(t *testing.T) {
	f := newFakeApple(t)
	f.script(
		apiErr(http.StatusTooManyRequests, 4290000, "Rate limit exceeded.", "Retry-After", "3"),
		ok(map[string]any{"testNotificationToken": "tok"}),
	)
	c, sl := f.client(WithClock(fixedClock))
	_, err := c.RequestTestNotification(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{3 * time.Second}, sl.all())
}

func TestRetry_LongRetryAfterIsReturnedNotSlept(t *testing.T) {
	f := newFakeApple(t)
	at := fixedNow.Add(time.Hour).UnixMilli()
	f.respond(apiErr(http.StatusTooManyRequests, 4290000, "Rate limit exceeded.", "Retry-After", strconv.FormatInt(at, 10)))
	c, sl := f.client(WithClock(fixedClock))

	_, err := c.RequestTestNotification(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRateLimited)
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, time.Hour, ae.RetryAfter)
	assert.Equal(t, 4290000, int(ae.Code))
	assert.Len(t, f.requests(), 1, "an hourly limit is not retried in-process")
	assert.Empty(t, sl.all())
}

func TestRetry_ServerErrorsBackOffThenGiveUp(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(http.StatusInternalServerError, 5000000, "An unknown error occurred."))
	c, sl := f.client(WithBackoff(time.Second), WithMaxRetries(3))

	_, err := c.RequestTestNotification(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrServer)
	assert.Len(t, f.requests(), 4, "one try and three retries")

	d := sl.all()
	require.Len(t, d, 3)
	for i, got := range d {
		full := time.Second << i
		assert.GreaterOrEqual(t, got, full/2, "retry %d waits at least half the backoff", i)
		assert.LessOrEqual(t, got, full, "retry %d waits at most the backoff", i)
	}
}

func TestRetry_BackoffIsCapped(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(http.StatusBadGateway, 0, ""))
	c, sl := f.client(WithBackoff(time.Second), WithMaxRetries(5), WithMaxRetryWait(3*time.Second))
	_, err := c.RequestTestNotification(context.Background())
	require.Error(t, err)
	for _, d := range sl.all() {
		assert.LessOrEqual(t, d, 3*time.Second)
	}
}

func TestRetry_NoRetriesWhenDisabled(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(http.StatusInternalServerError, 5000000, "x"))
	c, _ := f.client(WithMaxRetries(0))
	_, err := c.RequestTestNotification(context.Background())
	require.Error(t, err)
	assert.Len(t, f.requests(), 1)
}

func TestRetry_AppleTryAgainCodesAreRetried(t *testing.T) {
	for _, code := range []int64{4040002, 4040004, 4040006, 5000001} {
		t.Run(strconv.FormatInt(code, 10), func(t *testing.T) {
			f := newFakeApple(t)
			status := http.StatusNotFound
			if code >= 5000000 {
				status = http.StatusInternalServerError
			}
			f.script(apiErr(status, code, "Please try again."), ok(map[string]any{"signedTransactionInfo": f.ca.Sign(t, tx("1", nil))}))
			c, sl := f.client()
			got, err := c.GetTransactionInfo(context.Background(), "1")
			require.NoError(t, err)
			assert.Equal(t, "1", got.TransactionID)
			assert.Len(t, sl.all(), 1)
		})
	}
}

func TestRetry_PermanentErrorsAreNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   int64
		is     []error
	}{
		{"transaction not found", 404, 4040010, []error{ErrTransactionNotFound, ErrNotFound, entitlements.ErrNotFound}},
		{"original transaction not found", 404, 4040005, []error{ErrTransactionNotFound, entitlements.ErrNotFound}},
		{"account not found", 404, 4040001, []error{ErrAccountNotFound, entitlements.ErrNotFound}},
		{"app not found", 404, 4040003, []error{ErrAppNotFound, entitlements.ErrNotFound}},
		{"no notification url", 404, 4040007, []error{ErrNotificationURLNotFound, entitlements.ErrNotFound}},
		{"test notification not ready", 404, 4040008, []error{ErrTestNotificationNotReady, entitlements.ErrNotFound}},
		{"invalid transaction id", 400, 4000006, []error{ErrInvalidTransactionID, ErrBadRequest}},
		{"pagination token expired", 400, 4000017, []error{ErrPaginationTokenExpired, ErrBadRequest}},
		{"bad request", 400, 4000000, []error{ErrBadRequest}},
		{"unauthorized", 401, 0, []error{ErrUnauthorized}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApple(t)
			f.respond(apiErr(tc.status, tc.code, "nope"))
			c, sl := f.client()
			_, err := c.GetTransactionInfo(context.Background(), "1")
			require.Error(t, err)
			for _, target := range tc.is {
				assert.ErrorIs(t, err, target)
			}
			assert.NotErrorIs(t, err, ErrServer)
			assert.NotErrorIs(t, err, ErrRateLimited)
			assert.Len(t, f.requests(), 1)
			assert.Empty(t, sl.all())
			var ae *APIError
			require.ErrorAs(t, err, &ae)
			assert.Equal(t, tc.status, ae.StatusCode)
			assert.Equal(t, tc.code, ae.Code)
			assert.False(t, ae.Retryable())
		})
	}
}

func TestError_OtherClassesDoNotMatchNotFound(t *testing.T) {
	assert.NotErrorIs(t, &APIError{StatusCode: 400}, entitlements.ErrNotFound)
	assert.NotErrorIs(t, &APIError{StatusCode: 500}, ErrNotFound)
	assert.ErrorIs(t, &APIError{StatusCode: 429}, ErrRateLimited)
	assert.ErrorIs(t, &APIError{StatusCode: 503}, ErrServer)
}

func TestError_NonJSONBodyStillTyped(t *testing.T) {
	f := newFakeApple(t)
	f.respond(func(w http.ResponseWriter, _ recorded) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	c, _ := f.client(WithMaxRetries(0))
	_, err := c.GetTransactionInfo(context.Background(), "1")
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusBadGateway, ae.StatusCode)
	assert.Zero(t, ae.Code)
	assert.ErrorIs(t, err, ErrServer)
	assert.Contains(t, err.Error(), "HTTP 502")
}

func TestRetry_TransportErrorsAreRetried(t *testing.T) {
	f := newFakeApple(t)
	f.script(
		func(w http.ResponseWriter, _ recorded) {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
		},
		ok(map[string]any{"testNotificationToken": "tok"}),
	)
	c, sl := f.client()
	token, err := c.RequestTestNotification(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok", token)
	assert.Len(t, sl.all(), 1)
}

func TestRetry_StopsWhenContextIsCancelled(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(http.StatusServiceUnavailable, 0, ""))
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := f.client()
	c.sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }

	_, err := c.RequestTestNotification(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, ErrServer, "the cause stays visible")
	assert.Len(t, f.requests(), 1)
}

func TestRealSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	require.ErrorIs(t, sleepCtx(ctx, time.Minute), context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
	require.NoError(t, sleepCtx(context.Background(), time.Millisecond))
}

func TestParseRetryAfter(t *testing.T) {
	now := fixedNow
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{strconv.FormatInt(now.Add(1500*time.Millisecond).UnixMilli(), 10), 1500 * time.Millisecond, true},
		{strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10), 0, true},
		{"120", 2 * time.Minute, true},
		{"0", 0, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{"soon", 0, false},
		{"-5", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseRetryAfter(tc.in, now)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestErrorMessage(t *testing.T) {
	e := &APIError{Method: "GET", Path: "/inApps/v1/x", StatusCode: 429, Code: 4290000, Message: "Rate limit exceeded.", RetryAfter: 90 * time.Second}
	assert.Equal(t, "serverapi: GET /inApps/v1/x: HTTP 429 errorCode 4290000: Rate limit exceeded. (retry after 1m30s)", e.Error())
	assert.Equal(t, "serverapi: GET /x: HTTP 502", (&APIError{Method: "GET", Path: "/x", StatusCode: 502}).Error())
}

func TestErrorsJoinedWithUnwrapSlice(t *testing.T) {
	err := fmt.Errorf("outer: %w", &APIError{StatusCode: 404, Code: 4040010})
	assert.True(t, errors.Is(err, ErrTransactionNotFound))
}
