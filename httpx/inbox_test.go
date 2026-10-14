package httpx_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/apple/serverapi"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/inbox"
	"github.com/tggo/go-entitlements/inbox/memory"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
	"github.com/tggo/go-entitlements/internal/testutil"
)

// spy wraps the real in-memory inbox, recording what the handler passed to
// Save and letting a test fail or override it.
type spy struct {
	*memory.Inbox
	mu    sync.Mutex
	raws  []inbox.Raw
	calls int
	err   error
	id    string // fixed id to return, if set
}

func newSpy() *spy { return &spy{Inbox: memory.New()} }

func (f *spy) Save(ctx context.Context, raw inbox.Raw) (string, error) {
	f.mu.Lock()
	f.calls++
	raw.Body = bytes.Clone(raw.Body)
	f.raws = append(f.raws, raw)
	err, id := f.err, f.id
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return f.Inbox.Save(ctx, raw)
}

type stored struct {
	mu  sync.Mutex
	ids []string
}

func (s *stored) hook(_ context.Context, id string) {
	s.mu.Lock()
	s.ids = append(s.ids, id)
	s.mu.Unlock()
}
func (s *stored) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

type inboxCase struct {
	name string
	// build returns a handler wired with an inbox; parser and processor are
	// strict mocks that fail the test if the inbox handler touches them.
	build func(t *testing.T, opts ...httpx.Option) http.Handler
	src   entitlements.Source
}

func inboxCases() []inboxCase {
	return []inboxCase{
		{"apple", func(t *testing.T, opts ...httpx.Option) http.Handler {
			return mustApple(t, mocks.NewMockAppleParser(t), mocks.NewMockProcessor(t), append([]httpx.Option{httpx.WithLogger(testLogger(t))}, opts...)...)
		}, entitlements.SourceApple},
		{"google", func(t *testing.T, opts ...httpx.Option) http.Handler {
			return mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), opts...)
		}, entitlements.SourceGoogle},
	}
}

func req(method, path, body string, hdr map[string]string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestInbox_SavesRawAndAnswers200(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in, st := &spy{Inbox: memory.New(), id: "inbox-1"}, &stored{}
			h := c.build(t, httpx.WithInbox(in, st.hook), httpx.WithRoute(inbox.RouteProduction))
			before := time.Now()

			rec := serve(h, req(http.MethodPost, "/hooks/"+c.name, `{"raw":true}`, map[string]string{"Content-Type": "application/json", "User-Agent": "ua"}))

			assert.Equal(t, http.StatusOK, rec.Code)
			require.Len(t, in.raws, 1)
			raw := in.raws[0]
			assert.Equal(t, c.src, raw.Source)
			assert.Equal(t, inbox.RouteProduction, raw.Route, "default route")
			assert.Equal(t, `{"raw":true}`, string(raw.Body))
			assert.Equal(t, "application/json", http.Header(raw.Headers).Get("Content-Type"))
			assert.Equal(t, "ua", http.Header(raw.Headers).Get("User-Agent"))
			assert.WithinDuration(t, before, raw.ReceivedAt, 5*time.Second)
			assert.Equal(t, []string{"inbox-1"}, st.get(), "onStored runs once with the id Save returned")
		})
	}
}

func TestInbox_RouteOption(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := &spy{Inbox: memory.New(), id: "x"}
			h := c.build(t, httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction), httpx.WithRoute(inbox.RouteSandbox))
			serve(h, req(http.MethodPost, "/h", "b", nil))
			require.Len(t, in.raws, 1)
			assert.Equal(t, inbox.RouteSandbox, in.raws[0].Route)
		})
	}
}

// Credentials never reach the inbox. Pub/Sub's OIDC bearer token is the
// obvious one; cookies and key-like headers behind a proxy are the others.
func TestInbox_NeverStoresCredentials(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := &spy{Inbox: memory.New(), id: "x"}
			opts := []httpx.Option{httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction)}
			if c.src == entitlements.SourceGoogle {
				opts = append(opts, httpx.WithPushAuthenticator(func(*http.Request) error { return nil }))
			}
			h := c.build(t, opts...)
			hdr := map[string]string{
				"Authorization": "Bearer eyJsecret", "Proxy-Authorization": "Basic abc", "Cookie": "s=1",
				"X-Api-Key": "k", "X-Auth-Token": "t", "X-Forwarded-Secret": "s", "Content-Type": "application/json",
			}
			serve(h, req(http.MethodPost, "/h", "b", hdr))
			require.Len(t, in.raws, 1)
			for name := range hdr {
				if name == "Content-Type" {
					continue
				}
				assert.Empty(t, in.raws[0].Headers[name], "%s must not be stored", name)
			}
			assert.Equal(t, "application/json", http.Header(in.raws[0].Headers).Get("Content-Type"))
			for _, v := range in.raws[0].Headers {
				assert.NotContains(t, strings.Join(v, ","), "eyJsecret")
			}
		})
	}
}

// Stored headers are a copy: mutating the request afterwards or the stored
// map does not alias.
func TestInbox_HeadersAreACopy(t *testing.T) {
	t.Parallel()
	in := &spy{Inbox: memory.New(), id: "x"}
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction))
	r := req(http.MethodPost, "/h", "b", map[string]string{"Content-Type": "a"})
	serve(h, r)
	r.Header.Set("Content-Type", "changed")
	assert.Equal(t, "a", http.Header(in.raws[0].Headers).Get("Content-Type"))
}

// Verification that cannot be repeated later happens first.
func TestInbox_GoogleAuthenticatesBeforeSaving(t *testing.T) {
	t.Parallel()
	in := &spy{Inbox: memory.New(), id: "x"}
	st := &stored{}
	auth := func(*http.Request) error { return errors.New("bad token") }
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t),
		httpx.WithInbox(in, st.hook), httpx.WithRoute(inbox.RouteProduction), httpx.WithPushAuthenticator(auth))

	rec := serve(h, req(http.MethodPost, "/h", "b", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, in.calls, "nothing is saved for an unauthenticated push")
	assert.Empty(t, st.get())
}

func TestInbox_GoogleStillRequiresAuthOrOptOut(t *testing.T) {
	t.Parallel()
	_, err := httpx.NewGoogleHandler(nil, nil, httpx.WithInbox(newSpy(), nil), httpx.WithRoute(inbox.RouteProduction))
	assert.ErrorIs(t, err, httpx.ErrAuthRequired)
}

// With an inbox the handler does not parse or process, so those may be nil.
func TestInbox_NoParserOrProcessorNeeded(t *testing.T) {
	t.Parallel()
	in := &spy{Inbox: memory.New(), id: "x"}
	g, err := httpx.NewGoogleHandler(nil, nil, httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction), httpx.WithInsecureNoAuth())
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, serve(g, req(http.MethodPost, "/h", "b", nil)).Code)
	a := mustApple(t, nil, nil, httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction))
	assert.Equal(t, http.StatusOK, serve(a, req(http.MethodPost, "/h", "b", nil)).Code)
	assert.Len(t, in.raws, 2)
}

func TestInbox_StatusTable(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name   string
				method string
				body   string
				err    error
				code   int
				saves  int
				hook   int
			}{
				{"ok", http.MethodPost, "b", nil, http.StatusOK, 1, 1},
				{"unstorable body is 400, not a retry loop", http.MethodPost, "b", inbox.ErrInvalid, http.StatusBadRequest, 1, 0},
				{"GET is 405 and not saved", http.MethodGet, "", nil, http.StatusMethodNotAllowed, 0, 0},
				{"PUT is 405", http.MethodPut, "b", nil, http.StatusMethodNotAllowed, 0, 0},
				{"over the body limit is 413 and not saved", http.MethodPost, strings.Repeat("a", 9), nil, http.StatusRequestEntityTooLarge, 0, 0},
				{"exactly the limit is saved", http.MethodPost, strings.Repeat("a", 8), nil, http.StatusOK, 1, 1},
				{"save failure is 500 so the store retries", http.MethodPost, "b", errors.New("db down"), http.StatusInternalServerError, 1, 0},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					in, st := &spy{Inbox: memory.New(), id: "id", err: tc.err}, &stored{}
					h := c.build(t, httpx.WithInbox(in, st.hook), httpx.WithRoute(inbox.RouteProduction), httpx.WithMaxBodyBytes(8))

					rec := serve(h, req(tc.method, "/h", tc.body, nil))

					assert.Equal(t, tc.code, rec.Code)
					assert.Equal(t, tc.saves, in.calls)
					assert.Len(t, st.get(), tc.hook)
				})
			}
		})
	}
}

// onStored must never change the answer, even by panicking.
func TestInbox_OnStoredPanicDoesNotAffectTheAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			log, logs := newRecorder()
			in := &spy{Inbox: memory.New(), id: "x"}
			h := c.build(t, httpx.WithLogger(log), httpx.WithInbox(in, func(context.Context, string) { panic("hook blew up") }), httpx.WithRoute(inbox.RouteProduction))

			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("b"))
			require.NoError(t, err)
			resp, err := srv.Client().Do(r)
			require.NoError(t, err, "the connection survives")
			require.NoError(t, resp.Body.Close())

			assert.Equal(t, http.StatusOK, resp.StatusCode, "the delivery is stored, so the store must not retry it")
			var logged bool
			for _, rec := range logs() {
				logged = logged || (rec.Msg == "inbox onStored hook panicked" && strings.Contains(rec.Attrs["panic"].(string), "hook blew up"))
			}
			assert.True(t, logged, "got %v", logs())
		})
	}
}

// A panic in Save is a 500, like any other panic below the handler.
func TestInbox_PanicInSaveIs500(t *testing.T) {
	t.Parallel()
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithInbox(panicInbox{memory.New()}, nil), httpx.WithRoute(inbox.RouteProduction))
	assert.Equal(t, http.StatusInternalServerError, serve(h, req(http.MethodPost, "/h", "b", nil)).Code)
}

type panicInbox struct{ inbox.Inbox }

func (panicInbox) Save(context.Context, inbox.Raw) (string, error) { panic("save blew up") }

// onStored gets a context that outlives the request, so it can hand work to a
// background worker.
func TestInbox_OnStoredContextIsDetachedFromTheRequest(t *testing.T) {
	t.Parallel()
	var got context.Context
	in := &spy{Inbox: memory.New(), id: "x"}
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t),
		httpx.WithInbox(in, func(ctx context.Context, _ string) { got = ctx }), httpx.WithRoute(inbox.RouteProduction))
	ctx, cancel := context.WithCancel(context.Background())
	r := req(http.MethodPost, "/h", "b", nil).WithContext(ctx)

	serve(h, r)
	cancel()

	require.NotNil(t, got)
	assert.NoError(t, got.Err(), "cancelling the request does not cancel the hook's context")
}

// Documented composition: the consumption wrapper sits OUTSIDE the inbox
// handler. The real serverapi wrapper answers a verified CONSUMPTION_REQUEST
// itself and forwards everything else, with the body restored, to the inbox
// handler, which stores it unparsed.
func TestInbox_RealConsumptionWrapperSitsOutside(t *testing.T) {
	t.Parallel()
	const bundle = "com.example.app"
	ca := testutil.NewAppleCA(t)
	verifier, err := apple.NewVerifier(bundle, apple.WithRootPool(ca.Pool), apple.WithEnvironment(entitlements.EnvironmentSandbox))
	require.NoError(t, err)
	in := newSpy()
	inner := mustApple(t, nil, nil, httpx.WithLogger(testLogger(t)), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteSandbox))
	responded := 0
	wrapper := serverapi.NewConsumptionHandler(inner, serverapi.ConsumptionHandlerConfig{
		Verifier: verifier,
		// No consent: the answer is "send nothing", so no Apple call is made.
		Responder: serverapi.ConsumptionResponderFunc(func(context.Context, serverapi.ConsumptionRequest) (serverapi.ConsumptionInfo, error) {
			responded++
			return serverapi.ConsumptionInfo{}, serverapi.ErrConsentRequired
		}),
		Clock: func() time.Time { return time.UnixMilli(1791633600000).Add(time.Minute) },
	})
	signed := func(typ string) []byte {
		txn := map[string]any{
			"transactionId": "2000000000000002", "originalTransactionId": "1000000000000001", "bundleId": bundle,
			"productId": "com.example.app.monthly", "purchaseDate": 1791000000000, "expiresDate": 1793000000000,
			"type": "Auto-Renewable Subscription", "inAppOwnershipType": "PURCHASED", "signedDate": 1791633600000, "environment": "Sandbox",
		}
		payload := ca.Sign(t, map[string]any{
			"notificationType": typ, "notificationUUID": "uuid-" + typ, "version": "2.0", "signedDate": 1791633600000,
			"data": map[string]any{
				"bundleId": bundle, "environment": "Sandbox", "signedTransactionInfo": ca.Sign(t, txn),
				"consumptionRequestReason": "UNINTENDED_PURCHASE",
			},
		})
		return []byte(`{"signedPayload":"` + payload + `"}`)
	}

	assert.Equal(t, http.StatusOK, serve(wrapper, req(http.MethodPost, "/apple", string(signed("CONSUMPTION_REQUEST")), nil)).Code)
	renew := signed("DID_RENEW")
	assert.Equal(t, http.StatusOK, serve(wrapper, req(http.MethodPost, "/apple", string(renew), nil)).Code)

	assert.Equal(t, 1, responded, "the wrapper answered the consumption request")
	require.Len(t, in.raws, 1, "only the forwarded delivery reaches the inbox")
	assert.Equal(t, string(renew), string(in.raws[0].Body), "the body arrives unmodified")
	assert.Equal(t, inbox.RouteSandbox, in.raws[0].Route)
}

// Inside the wrapper is wrong: the inbox handler would store the consumption
// request and answer 200, and nobody would answer it to Apple. Pin the
// documented order by showing what the reverse does.
func TestInbox_ConsumptionWrapperInsideWouldSwallowTheRequest(t *testing.T) {
	t.Parallel()
	in := newSpy()
	h := mustApple(t, nil, nil, httpx.WithLogger(testLogger(t)), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction))
	assert.Equal(t, http.StatusOK, serve(h, req(http.MethodPost, "/apple", `{"signedPayload":"a.b.c"}`, nil)).Code)
	assert.Len(t, in.raws, 1, "an inbox handler stores whatever reaches it, so it must be behind the wrapper")
}

func TestInbox_RouteIsRequiredAndMustBeAConstant(t *testing.T) {
	t.Parallel()
	in := newSpy()
	newApple := func(o ...httpx.Option) error {
		_, err := httpx.NewAppleHandler(nil, nil, o...)
		return err
	}
	newGoogle := func(o ...httpx.Option) error {
		_, err := httpx.NewGoogleHandler(nil, nil, append([]httpx.Option{httpx.WithInsecureNoAuth()}, o...)...)
		return err
	}
	for name, build := range map[string]func(...httpx.Option) error{"apple": newApple, "google": newGoogle} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, build(httpx.WithInbox(in, nil)), httpx.ErrRouteRequired)
			require.ErrorIs(t, build(httpx.WithInbox(in, nil), httpx.WithRoute("/some/path")), httpx.ErrRouteInvalid)
			for _, route := range []inbox.Route{inbox.RouteProduction, inbox.RouteSandbox, inbox.RouteHistory, inbox.RouteReconcile} {
				require.NoError(t, build(httpx.WithInbox(in, nil), httpx.WithRoute(route)), route)
			}
		})
	}
}

// The route is validated by the constructor only when an inbox is set: without
// one it is meaningless and ignored.
func TestInbox_RouteWithoutInboxIsIgnored(t *testing.T) {
	t.Parallel()
	_, err := httpx.NewAppleHandler(nil, nil, httpx.WithRoute("whatever"))
	assert.NoError(t, err)
}

// inbox.Validate runs before Save, so an item no Inbox could keep never
// reaches it.
func TestInbox_ValidatesBeforeSaving(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := newSpy()
			h := c.build(t, httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction), httpx.WithMaxBodyBytes(inbox.MaxBodyBytes+10))

			rec := serve(h, req(http.MethodPost, "/h", strings.Repeat("a", inbox.MaxBodyBytes+1), nil))

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, in.calls, "Save is not called for an oversized body")
		})
	}
}

func TestInbox_NilInboxOptionIsIgnored(t *testing.T) {
	t.Parallel()
	parser := mocks.NewMockAppleParser(t)
	parser.EXPECT().Parse(mock.Anything).Return(entitlements.Event{}, entitlements.ErrIgnored)
	h := mustApple(t, parser, mocks.NewMockProcessor(t), httpx.WithInbox(nil, nil), httpx.WithRoute(inbox.RouteProduction), httpx.WithLogger(testLogger(t)))
	assert.Equal(t, http.StatusOK, serve(h, req(http.MethodPost, "/h", "b", nil)).Code, "without an inbox the handler parses as usual")
}

// The real inbox rejects an empty body; the handler must not turn that into an
// endless 500 retry.
func TestInbox_EmptyBodyIs400(t *testing.T) {
	t.Parallel()
	for _, c := range inboxCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in, st := newSpy(), &stored{}
			h := c.build(t, httpx.WithInbox(in, st.hook), httpx.WithRoute(inbox.RouteProduction))
			assert.Equal(t, http.StatusBadRequest, serve(h, req(http.MethodPost, "/h", "", nil)).Code)
			assert.Empty(t, st.get())
		})
	}
}

// Google: the Pub/Sub messageId is the idempotency key, but only once push
// authentication has passed.
func TestInbox_GoogleRawIDIsTheMessageIDOnlyWhenAuthenticated(t *testing.T) {
	t.Parallel()
	body := `{"message":{"messageId":"12345","data":"e30="},"subscription":"s"}`
	authOK := httpx.WithPushAuthenticator(func(*http.Request) error { return nil })

	t.Run("authenticated: id is the messageId and a redelivery is one item", func(t *testing.T) {
		t.Parallel()
		in := newSpy()
		h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction), authOK)
		serve(h, req(http.MethodPost, "/h", body, nil))
		serve(h, req(http.MethodPost, "/h", body, nil))
		require.Len(t, in.raws, 2)
		assert.Equal(t, "12345", in.raws[0].ID)
		items, err := in.List(context.Background(), inbox.Filter{})
		require.NoError(t, err)
		assert.Len(t, items, 1, "the same Pub/Sub delivery twice is stored once")
	})
	t.Run("no authenticator: an unauthenticated body must not pick its own id", func(t *testing.T) {
		t.Parallel()
		in := newSpy()
		h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction))
		serve(h, req(http.MethodPost, "/h", body, nil))
		require.Len(t, in.raws, 1)
		assert.Empty(t, in.raws[0].ID)
	})
	for name, bad := range map[string]string{
		"not json":      `not json`,
		"no message id": `{"message":{"data":"e30="}}`,
		"numeric id":    `{"message":{"messageId":12345}}`,
		"control char":  "{\"message\":{\"messageId\":\"a\\u0000b\"}}",
		"overlong id":   `{"message":{"messageId":"` + strings.Repeat("a", 300) + `"}}`,
	} {
		t.Run("authenticated but "+name+": no id", func(t *testing.T) {
			t.Parallel()
			in := newSpy()
			h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction), authOK)
			serve(h, req(http.MethodPost, "/h", bad, nil))
			require.Len(t, in.raws, 1)
			assert.Empty(t, in.raws[0].ID)
		})
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// Apple: Raw.ID stays empty. If it were taken from the notificationUUID of the
// still-unverified JWS, a forged body naming a real notification's UUID would
// claim that id first, and the genuine delivery would be dropped as a repeat.
func TestInbox_ForgedAppleBodyCannotBlockTheGenuineOne(t *testing.T) {
	t.Parallel()
	ca := testutil.NewAppleCA(t)
	genuine := testutil.LoadAppleFixture(t, "subscribed").SignBody(t, ca)

	verifier, err := apple.NewVerifier("", apple.WithRootPool(ca.Pool), apple.WithInsecureAnyBundle())
	require.NoError(t, err)
	parser := apple.NewParser(verifier)
	ev, err := parser.Parse(genuine)
	require.NoError(t, err)
	realUUID := ev.ID
	require.NotEmpty(t, realUUID)

	// The attacker knows the UUID (it was in a log or a screenshot) and sends it
	// in an unsigned body first.
	forged := []byte(`{"signedPayload":"` + b64(`{"alg":"ES256"}`) + `.` + b64(`{"notificationUUID":"`+realUUID+`"}`) + `.` + b64("sig") + `"}`)

	in := newSpy()
	h := mustApple(t, mocks.NewMockAppleParser(t), mocks.NewMockProcessor(t), httpx.WithLogger(testLogger(t)), httpx.WithInbox(in, nil), httpx.WithRoute(inbox.RouteProduction))
	require.Equal(t, http.StatusOK, serve(h, req(http.MethodPost, "/h", string(forged), nil)).Code)
	require.Equal(t, http.StatusOK, serve(h, req(http.MethodPost, "/h", string(genuine), nil)).Code)

	require.Len(t, in.raws, 2)
	for _, raw := range in.raws {
		assert.Empty(t, raw.ID, "the handler must not derive an id from an unverified body")
	}
	items, err := in.List(context.Background(), inbox.Filter{})
	require.NoError(t, err)
	require.Len(t, items, 2, "both deliveries are kept; the forged one did not claim the real UUID")
	assert.NotEqual(t, items[0].ID, items[1].ID)

	// Verification happens later, per item: the forged one fails, the genuine one verifies.
	var verified int
	for _, it := range items {
		if _, perr := parser.Parse(it.Body); perr == nil {
			verified++
		} else {
			assert.ErrorIs(t, perr, entitlements.ErrInvalidSignature)
		}
	}
	assert.Equal(t, 1, verified)
}
