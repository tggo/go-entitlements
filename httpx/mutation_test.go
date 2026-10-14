package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
)

func wrap(err error) error { return fmt.Errorf("w: %w", err) }

// Every sentinel the parsers can return, with the exact status Pub/Sub or
// Apple will see.
func TestGoogleHandler_ParseErrorStatusTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"ErrIgnored", wrap(entitlements.ErrIgnored), http.StatusOK},
		{"ErrUnsupported", wrap(entitlements.ErrUnsupported), http.StatusOK},
		{"ErrNoExpiry", wrap(google.ErrNoExpiry), http.StatusOK},
		{"ErrTestNotification", wrap(google.ErrTestNotification), http.StatusOK},
		{"ErrTestPurchase", wrap(google.ErrTestPurchase), http.StatusOK},
		{"ErrPackageMismatch", wrap(google.ErrPackageMismatch), http.StatusOK},
		{"ErrPurchaseInvalid", wrap(google.ErrPurchaseInvalid), http.StatusOK},
		{"ErrPurchaseNotFound", wrap(google.ErrPurchaseNotFound), http.StatusOK},
		{"ErrFetchConfig", wrap(google.ErrFetchConfig), http.StatusInternalServerError},
		{"ErrFetchTransient", wrap(google.ErrFetchTransient), http.StatusInternalServerError},
		{"ErrMalformed", wrap(google.ErrMalformed), http.StatusBadRequest},
		{"ErrInvalidSignature", wrap(entitlements.ErrInvalidSignature), http.StatusUnauthorized},
		{"unclassified", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(gres(entitlements.Event{}), tc.err)
			h := mustGoogle(t, parser, mocks.NewMockProcessor(t), testLogger(t))
			assert.Equal(t, tc.code, post(h, "body").Code)
		})
	}
}

func TestGoogleHandler_ProcessErrorStatusTable(t *testing.T) {
	t.Parallel()
	ev := entitlements.Event{ID: "m", Source: entitlements.SourceGoogle, OriginalTxID: "t", State: entitlements.StateActive}
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"applied", nil, http.StatusOK},
		{"ErrDuplicate", wrap(entitlements.ErrDuplicate), http.StatusOK},
		{"ErrStale", wrap(entitlements.ErrStale), http.StatusOK},
		{"ErrIgnored", wrap(entitlements.ErrIgnored), http.StatusOK},
		{"ErrUnsupported", wrap(entitlements.ErrUnsupported), http.StatusOK},
		{"ErrInvalidEvent", wrap(entitlements.ErrInvalidEvent), http.StatusOK},
		{"store failure", errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(gres(ev), nil)
			svc := mocks.NewMockProcessor(t)
			svc.EXPECT().Process(mock.Anything, ev).Return(&entitlements.Entitlement{State: entitlements.StateActive}, tc.err)
			h := mustGoogle(t, parser, svc, testLogger(t))
			assert.Equal(t, tc.code, post(h, "body").Code)
		})
	}
}

func TestAppleHandler_ParseErrorStatusTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"ErrIgnored", wrap(entitlements.ErrIgnored), http.StatusOK},
		{"ErrUnsupported", wrap(entitlements.ErrUnsupported), http.StatusOK},
		{"ErrInvalidSignature", wrap(entitlements.ErrInvalidSignature), http.StatusUnauthorized},
		{"anything else is malformed", errors.New("bad jws"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parser := mocks.NewMockAppleParser(t)
			parser.EXPECT().Parse([]byte("body")).Return(entitlements.Event{}, tc.err)
			h := httpx.NewAppleHandler(parser, mocks.NewMockProcessor(t), httpx.WithLogger(testLogger(t)))
			assert.Equal(t, tc.code, post(h, "body").Code)
		})
	}
}

func TestMethodCheckComesBeforeAuthentication(t *testing.T) {
	t.Parallel()
	called := false
	auth := func(*http.Request) error { called = true; return errors.New("denied") }
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithPushAuthenticator(auth))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.False(t, called, "authenticator must not run for a non-POST")
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
}

func TestFailedAuthenticationIs401AndSkipsParsing(t *testing.T) {
	t.Parallel()
	auth := func(*http.Request) error { return errors.New("denied") }
	h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithPushAuthenticator(auth))
	assert.Equal(t, http.StatusUnauthorized, post(h, "body").Code)
}

// The default limit is exactly 1 MiB: that passes, one byte more is 413.
func TestDefaultMaxBodyBoundary(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(1<<20), int64(httpx.DefaultMaxBodyBytes))
	atLimit := string(bytes.Repeat([]byte("a"), httpx.DefaultMaxBodyBytes))
	overLimit := atLimit + "a"

	t.Run("google", func(t *testing.T) {
		t.Parallel()
		parser := mocks.NewMockGoogleParser(t)
		parser.EXPECT().ParseResult(mock.Anything, []byte(atLimit)).Return(gres(entitlements.Event{}), entitlements.ErrIgnored).Once()
		h := mustGoogle(t, parser, mocks.NewMockProcessor(t), testLogger(t))
		assert.Equal(t, http.StatusOK, post(h, atLimit).Code)
		assert.Equal(t, http.StatusRequestEntityTooLarge, post(h, overLimit).Code)
	})
	t.Run("apple", func(t *testing.T) {
		t.Parallel()
		parser := mocks.NewMockAppleParser(t)
		parser.EXPECT().Parse([]byte(atLimit)).Return(entitlements.Event{}, entitlements.ErrIgnored).Once()
		h := httpx.NewAppleHandler(parser, mocks.NewMockProcessor(t), httpx.WithLogger(testLogger(t)))
		assert.Equal(t, http.StatusOK, post(h, atLimit).Code)
		assert.Equal(t, http.StatusRequestEntityTooLarge, post(h, overLimit).Code)
	})
}
