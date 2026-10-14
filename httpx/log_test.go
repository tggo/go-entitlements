package httpx_test

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/httpx"
)

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

// recorder is a slog.Handler that keeps every record for assertions.
type recorder struct {
	mu   *sync.Mutex
	recs *[]logRecord
	pre  []slog.Attr
}

func newRecorder() (*slog.Logger, func() []logRecord) {
	r := &recorder{mu: &sync.Mutex{}, recs: &[]logRecord{}}
	return slog.New(r), func() []logRecord {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]logRecord(nil), *r.recs...)
	}
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]any{}
	for _, a := range r.pre {
		attrs[a.Key] = a.Value.Any()
	}
	rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.recs = append(*r.recs, logRecord{Level: rec.Level, Msg: rec.Message, Attrs: attrs})
	return nil
}

func (r *recorder) WithAttrs(a []slog.Attr) slog.Handler {
	return &recorder{mu: r.mu, recs: r.recs, pre: append(append([]slog.Attr(nil), r.pre...), a...)}
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (l logRecord) is(level slog.Level, msg string) bool { return l.Level == level && l.Msg == msg }

// testLogger logs to the test output.
func testLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return slog.New(slog.NewTextHandler(testWriter{tb}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// mustGoogle builds a Google handler without push auth (tests exercise the
// response policy, not authentication).
func mustGoogle(tb testing.TB, p httpx.GoogleParser, svc httpx.Processor, log *slog.Logger, opts ...httpx.Option) http.Handler {
	tb.Helper()
	opts = append([]httpx.Option{httpx.WithLogger(log), httpx.WithInsecureNoAuth()}, opts...)
	h, err := httpx.NewGoogleHandler(p, svc, opts...)
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

// gres wraps one event in a parse result.
func gres(ev entitlements.Event) google.Result {
	return google.Result{Events: []entitlements.Event{ev}}
}
