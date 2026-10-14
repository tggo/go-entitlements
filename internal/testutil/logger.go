package testutil

import (
	"io"
	"log/slog"
	"testing"
)

type testWriter struct{ tb testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.tb.Helper()
	w.tb.Log(string(p))
	return len(p), nil
}

var _ io.Writer = testWriter{}

// NewLogger returns a *slog.Logger that writes to tb.Log.
func NewLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return slog.New(slog.NewTextHandler(testWriter{tb}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
