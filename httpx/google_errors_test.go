package httpx_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
	"github.com/tggo/go-entitlements/httpx/mocks"
)

func TestGoogleHandler_FetchErrorClasses(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCode  int
		wantLevel zapcore.Level
		wantMsg   string
	}{
		{"purchase gone is acknowledged", fmt.Errorf("w: %w", google.ErrPurchaseNotFound), http.StatusOK, zapcore.WarnLevel, "purchase token unknown or expired"},
		{"config error is loud 500", fmt.Errorf("w: %w", google.ErrFetchConfig), http.StatusInternalServerError, zapcore.ErrorLevel, "play api credentials or permissions rejected"},
		{"transient is retryable 500", fmt.Errorf("w: %w", google.ErrFetchTransient), http.StatusInternalServerError, zapcore.ErrorLevel, "play api transient failure"},
		{"unclassified stays 500", errors.New("boom"), http.StatusInternalServerError, zapcore.ErrorLevel, "parse push"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().Parse(mock.Anything, []byte("body")).Return(entitlements.Event{}, tt.err)
			h := httpx.NewGoogleHandler(parser, mocks.NewMockProcessor(t), zap.New(core))

			assert.Equal(t, tt.wantCode, post(h, "body").Code)

			entries := logs.FilterMessage(tt.wantMsg).All()
			if assert.Len(t, entries, 1, "log message %q", tt.wantMsg) {
				assert.Equal(t, tt.wantLevel, entries[0].Level)
			}
		})
	}
}
