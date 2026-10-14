package httpx_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
)

// Security H1: no authenticator is an explicit decision, not a default.
func TestNewGoogleHandler_RequiresAuthOrExplicitOptOut(t *testing.T) {
	t.Parallel()
	parser, svc := mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t)

	h, err := httpx.NewGoogleHandler(parser, svc)
	require.ErrorIs(t, err, httpx.ErrAuthRequired)
	assert.Nil(t, h)

	h, err = httpx.NewGoogleHandler(parser, svc, httpx.WithPushAuthenticator(func(*http.Request) error { return nil }))
	require.NoError(t, err)
	assert.NotNil(t, h)
}

func TestNewGoogleHandler_InsecureOptOutLogsAnError(t *testing.T) {
	t.Parallel()
	log, logs := newRecorder()

	h, err := httpx.NewGoogleHandler(mocks.NewMockGoogleParser(t), mocks.NewMockProcessor(t),
		httpx.WithLogger(log), httpx.WithInsecureNoAuth())

	require.NoError(t, err)
	assert.NotNil(t, h)
	var loud bool
	for _, r := range logs() {
		loud = loud || r.Level == slog.LevelError
	}
	assert.True(t, loud, "running without push authentication must log at error level, got %v", logs())
}

func TestNewGoogleHandler_NilParserOrProcessor(t *testing.T) {
	t.Parallel()
	_, err := httpx.NewGoogleHandler(nil, mocks.NewMockProcessor(t), httpx.WithInsecureNoAuth())
	require.ErrorIs(t, err, httpx.ErrDependencyRequired)
	_, err = httpx.NewGoogleHandler(mocks.NewMockGoogleParser(t), nil, httpx.WithInsecureNoAuth())
	require.ErrorIs(t, err, httpx.ErrDependencyRequired)
}

// Security M1: a token Play calls invalid will never become valid.
func TestInvalidPurchaseTokenIsAcknowledgedLoudly(t *testing.T) {
	t.Parallel()
	log, logs := newRecorder()
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(entitlements.Event{}, fmt.Errorf("w: %w", google.ErrPurchaseInvalid))
	h := mustGoogle(t, parser, mocks.NewMockProcessor(t), log)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)

	var loud bool
	for _, r := range logs() {
		loud = loud || r.Level == slog.LevelError
	}
	assert.True(t, loud, "got %v", logs())
}

// Maintainer: one request, one log record, at the right level.
func TestOneRecordPerRequest(t *testing.T) {
	t.Parallel()
	ev := entitlements.Event{ID: "m1", Source: entitlements.SourceGoogle, OriginalTxID: "tok", State: entitlements.StateActive}
	cases := []struct {
		name      string
		parseErr  error
		procErr   error
		process   bool
		wantLevel slog.Level
	}{
		{"applied", nil, nil, true, slog.LevelInfo},
		{"duplicate", nil, entitlements.ErrDuplicate, true, slog.LevelInfo},
		{"stale", nil, entitlements.ErrStale, true, slog.LevelInfo},
		{"play console test ping", fmt.Errorf("w: %w", google.ErrTestNotification), nil, false, slog.LevelDebug},
		{"ignored", entitlements.ErrIgnored, nil, false, slog.LevelInfo},
		{"unsupported", entitlements.ErrUnsupported, nil, false, slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, logs := newRecorder()
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(ev, tc.parseErr)
			svc := mocks.NewMockProcessor(t)
			if tc.process {
				svc.EXPECT().Process(mock.Anything, ev).Return(&entitlements.Entitlement{State: entitlements.StateActive}, tc.procErr)
			}
			h := mustGoogle(t, parser, svc, log)

			require.Equal(t, http.StatusOK, post(h, "body").Code)

			recs := logs()
			// The insecure opt-out in mustGoogle logs once at construction.
			var perRequest []logRecord
			for _, r := range recs {
				if r.Msg != "push authentication disabled" {
					perRequest = append(perRequest, r)
				}
			}
			require.Len(t, perRequest, 1, "%v", recs)
			assert.Equal(t, tc.wantLevel, perRequest[0].Level)
		})
	}
}

func TestWithLoggerNilKeepsDiscard(t *testing.T) {
	t.Parallel()
	parser := mocks.NewMockAppleParser(t)
	parser.EXPECT().Parse(mock.Anything).Return(entitlements.Event{}, errors.New("boom"))
	h := httpx.NewAppleHandler(parser, mocks.NewMockProcessor(t), httpx.WithLogger(nil), nil)

	assert.Equal(t, http.StatusBadRequest, post(h, "x").Code)
}
