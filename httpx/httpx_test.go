package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
)

var testEvent = entitlements.Event{ID: "e1", Source: entitlements.SourceApple, OriginalTxID: "tx", State: entitlements.StateActive}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

func TestAppleHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		parseEv  entitlements.Event
		parseErr error
		process  bool
		procErr  error
		wantCode int
	}{
		{name: "applied", parseEv: testEvent, process: true, wantCode: 200},
		{name: "duplicate", parseEv: testEvent, process: true, procErr: entitlements.ErrDuplicate, wantCode: 200},
		{name: "stale", parseEv: testEvent, process: true, procErr: fmt.Errorf("x: %w", entitlements.ErrStale), wantCode: 200},
		{name: "process ignored", parseEv: testEvent, process: true, procErr: entitlements.ErrIgnored, wantCode: 200},
		{name: "process unsupported", parseEv: testEvent, process: true, procErr: entitlements.ErrUnsupported, wantCode: 200},
		{name: "store error", parseEv: testEvent, process: true, procErr: errors.New("db down"), wantCode: 500},
		{name: "parse ignored (TEST)", parseErr: entitlements.ErrIgnored, wantCode: 200},
		{name: "parse unsupported", parseErr: fmt.Errorf("w: %w", entitlements.ErrUnsupported), wantCode: 200},
		{name: "invalid signature", parseErr: fmt.Errorf("w: %w", entitlements.ErrInvalidSignature), wantCode: 401},
		{name: "malformed", parseErr: errors.New("apple: decode body"), wantCode: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parser := mocks.NewMockAppleParser(t)
			svc := mocks.NewMockProcessor(t)
			parser.EXPECT().Parse([]byte("body")).Return(tt.parseEv, tt.parseErr)
			if tt.process {
				ent := &entitlements.Entitlement{State: entitlements.StateActive}
				svc.EXPECT().Process(mock.Anything, tt.parseEv).Return(ent, tt.procErr)
			}
			h := httpx.NewAppleHandler(parser, svc, httpx.WithLogger(testLogger(t)))
			assert.Equal(t, tt.wantCode, post(h, "body").Code)
		})
	}
}

func TestHandlers_MethodAndBodyLimit(t *testing.T) {
	t.Parallel()
	apple := httpx.NewAppleHandler(mocks.NewMockAppleParser(t), mocks.NewMockProcessor(t), httpx.WithLogger(testLogger(t)), httpx.WithMaxBodyBytes(4))
	google := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithMaxBodyBytes(4))
	for name, h := range map[string]http.Handler{"apple": apple, "google": google} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Equal(t, "POST", rec.Header().Get("Allow"))

			assert.Equal(t, http.StatusRequestEntityTooLarge, post(h, "12345").Code)
		})
	}
}

func TestGoogleHandler(t *testing.T) {
	t.Parallel()
	gev := entitlements.Event{ID: "m1", Source: entitlements.SourceGoogle, OriginalTxID: "tok", State: entitlements.StateActive}
	tests := []struct {
		name     string
		parseEv  entitlements.Event
		parseErr error
		process  bool
		procErr  error
		wantCode int
	}{
		{name: "applied", parseEv: gev, process: true, wantCode: 200},
		{name: "duplicate", parseEv: gev, process: true, procErr: entitlements.ErrDuplicate, wantCode: 200},
		{name: "stale", parseEv: gev, process: true, procErr: entitlements.ErrStale, wantCode: 200},
		{name: "store error", parseEv: gev, process: true, procErr: errors.New("db down"), wantCode: 500},
		{name: "test notification", parseErr: entitlements.ErrIgnored, wantCode: 200},
		{name: "unsupported", parseErr: fmt.Errorf("w: %w", entitlements.ErrUnsupported), wantCode: 200},
		{name: "other package", parseErr: fmt.Errorf("w: %w", google.ErrPackageMismatch), wantCode: 200},
		{name: "malformed", parseErr: fmt.Errorf("w: %w", google.ErrMalformed), wantCode: 400},
		{name: "fetch failed", parseErr: errors.New("google: fetch subscription: 503"), wantCode: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parser := mocks.NewMockGoogleParser(t)
			svc := mocks.NewMockProcessor(t)
			parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(tt.parseEv, tt.parseErr)
			if tt.process {
				ent := &entitlements.Entitlement{State: entitlements.StateActive}
				svc.EXPECT().Process(mock.Anything, tt.parseEv).Return(ent, tt.procErr)
			}
			h := mustGoogle(t, parser, svc, testLogger(t))
			assert.Equal(t, tt.wantCode, post(h, "body").Code)
		})
	}
}

func TestGoogleHandler_PushAuthenticator(t *testing.T) {
	t.Parallel()
	t.Run("rejected before body is parsed", func(t *testing.T) {
		t.Parallel()
		auth := func(*http.Request) error { return errors.New("bad token") }
		h := mustGoogle(t, mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t), testLogger(t), httpx.WithPushAuthenticator(auth))
		assert.Equal(t, http.StatusUnauthorized, post(h, "body").Code)
	})
	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		auth := func(*http.Request) error { return nil }
		parser := mocks.NewMockGoogleParser(t)
		parser.EXPECT().Parse(mock.Anything, mock.Anything).Return(entitlements.Event{}, entitlements.ErrIgnored)
		h := mustGoogle(t, parser, mocks.NewMockProcessor(t), testLogger(t), httpx.WithPushAuthenticator(auth))
		assert.Equal(t, http.StatusOK, post(h, "body").Code)
	})
}

func TestNilLogger(t *testing.T) {
	t.Parallel()
	parser := mocks.NewMockAppleParser(t)
	parser.EXPECT().Parse(mock.Anything).Return(entitlements.Event{}, entitlements.ErrIgnored)
	h := httpx.NewAppleHandler(parser, mocks.NewMockProcessor(t), httpx.WithLogger(nil))
	require.Equal(t, http.StatusOK, post(h, "x").Code)
}
