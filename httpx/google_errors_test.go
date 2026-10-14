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

func TestGoogleHandler_FetchErrorClasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		wantCode  int
		wantLevel slog.Level
		wantMsg   string
	}{
		{"purchase gone is acknowledged", fmt.Errorf("w: %w", google.ErrPurchaseNotFound), http.StatusOK, slog.LevelWarn, "purchase token unknown or expired"},
		{"config error is loud 500", fmt.Errorf("w: %w", google.ErrFetchConfig), http.StatusInternalServerError, slog.LevelError, "play api credentials or permissions rejected"},
		{"transient is retryable 500", fmt.Errorf("w: %w", google.ErrFetchTransient), http.StatusInternalServerError, slog.LevelError, "play api transient failure"},
		{"unclassified stays 500", errors.New("boom"), http.StatusInternalServerError, slog.LevelError, "parse push"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			log, logs := newRecorder()
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(entitlements.Event{}, tt.err)
			h := mustGoogle(t, parser, mocks.NewMockProcessor(t), log)

			assert.Equal(t, tt.wantCode, post(h, "body").Code)

			var found int
			for _, rec := range logs() {
				if rec.is(tt.wantLevel, tt.wantMsg) {
					found++
				}
			}
			assert.Equal(t, 1, found, "log %v %q in %v", tt.wantLevel, tt.wantMsg, logs())
		})
	}
}
