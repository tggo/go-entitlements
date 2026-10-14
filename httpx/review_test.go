package httpx_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
)

func TestProcessorReturningNilNil(t *testing.T) {
	t.Parallel()
	ev := entitlements.Event{ID: "m1", Source: entitlements.SourceGoogle, OriginalTxID: "tok", State: entitlements.StateActive}
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(ev, nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, ev).Return(nil, nil)
	h := mustGoogle(t, parser, svc, testLogger(t))

	assert.NotPanics(t, func() { assert.Equal(t, http.StatusOK, post(h, "body").Code) })
}

func TestInvalidEventIsAcknowledgedLoudly(t *testing.T) {
	t.Parallel()
	ev := entitlements.Event{ID: "m1", Source: entitlements.SourceGoogle}
	log, logs := newRecorder()
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(ev, nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, ev).Return(nil, fmt.Errorf("w: %w", entitlements.ErrInvalidEvent))
	h := mustGoogle(t, parser, svc, log)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)

	var loud bool
	for _, r := range logs() {
		loud = loud || (r.Level == slog.LevelError && r.Attrs["event"] == "m1")
	}
	assert.True(t, loud, "expected an error-level record, got %v", logs())
}

func TestNoExpiryIsLoud(t *testing.T) {
	t.Parallel()
	log, logs := newRecorder()
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(entitlements.Event{}, fmt.Errorf("w: %w", google.ErrNoExpiry))
	h := mustGoogle(t, parser, mocks.NewMockProcessor(t), log)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)

	var loud bool
	for _, r := range logs() {
		loud = loud || r.Level == slog.LevelError
	}
	assert.True(t, loud, "a dropped granting event must log at error level, got %v", logs())
}

func TestNilLoggerAndNilOptionAreSafe(t *testing.T) {
	t.Parallel()
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().Parse(mock.Anything, mock.Anything).Return(entitlements.Event{}, errors.New("boom"))
	h := mustGoogle(t, parser, mocks.NewMockProcessor(t), nil)

	assert.Equal(t, http.StatusInternalServerError, post(h, "body").Code)
}
