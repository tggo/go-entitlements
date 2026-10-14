package httpx_test

import (
	"context"
	"errors"
	"log/slog"
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
	gmocks "github.com/tggo/go-entitlements/internal/mocks/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/httpx"
)

func evFor(id, tx string) entitlements.Event {
	return entitlements.Event{ID: id, Source: entitlements.SourceGoogle, OriginalTxID: tx, State: entitlements.StateActive}
}

// A subscription with add-ons is several events in one push: all are applied,
// in order, and the push gets one answer.
func TestMultiEvent_AllAppliedInOrder(t *testing.T) {
	t.Parallel()
	a, b, c := evFor("m#base", "t#base"), evFor("m#addon", "t#addon"), evFor("m#retire", "old")
	log := &steps{}
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(google.Result{Events: []entitlements.Event{a, b, c}}, nil)
	svc := mocks.NewMockProcessor(t)
	for _, e := range []entitlements.Event{a, b, c} {
		svc.EXPECT().Process(mock.Anything, e).RunAndReturn(func(_ context.Context, ev entitlements.Event) (*entitlements.Entitlement, error) {
			log.add(ev.ID)
			return granted, nil
		})
	}
	h := mustGoogle(t, parser, svc, testLogger(t))

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"m#base", "m#addon", "m#retire"}, log.get())
}

// One retryable failure makes the push a 500, but the other events are still
// applied, and nothing is acknowledged.
func TestMultiEvent_OneFailureIs500AndTheRestStillApply(t *testing.T) {
	t.Parallel()
	a, b, c := evFor("m#base", "t#base"), evFor("m#addon", "t#addon"), evFor("m#third", "t#third")
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(
		google.Result{Events: []entitlements.Event{a, b, c}, Ack: &google.Ack{PackageName: "p", SubscriptionID: "s", PurchaseToken: "t"}}, nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, a).Return(granted, nil)
	svc.EXPECT().Process(mock.Anything, b).Return(nil, errors.New("db down"))
	svc.EXPECT().Process(mock.Anything, c).Return(granted, nil)
	acker := gmocks.NewMockAcknowledger(t) // an acknowledge would fail the test
	h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(testLogger(t)), httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(acker))
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, post(h, "body").Code)
}

func TestMultiEvent_PermanentOutcomesStay200(t *testing.T) {
	t.Parallel()
	a, b := evFor("m#base", "t#base"), evFor("m#addon", "t#addon")
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(google.Result{Events: []entitlements.Event{a, b}}, nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, a).Return(nil, entitlements.ErrDuplicate)
	svc.EXPECT().Process(mock.Anything, b).Return(nil, entitlements.ErrInvalidEvent)
	h := mustGoogle(t, parser, svc, testLogger(t))

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
}

func TestMultiEvent_AcknowledgesOnceAfterAllAreStored(t *testing.T) {
	t.Parallel()
	a, b := evFor("m#base", "t#base"), evFor("m#addon", "t#addon")
	ack := &google.Ack{PackageName: "p", SubscriptionID: "s", PurchaseToken: "t"}
	log := &steps{}
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(google.Result{Events: []entitlements.Event{a, b}, Ack: ack}, nil)
	svc := mocks.NewMockProcessor(t)
	for _, e := range []entitlements.Event{a, b} {
		svc.EXPECT().Process(mock.Anything, e).RunAndReturn(func(_ context.Context, ev entitlements.Event) (*entitlements.Entitlement, error) {
			log.add("process " + ev.ID)
			return granted, nil
		})
	}
	acker := gmocks.NewMockAcknowledger(t)
	acker.EXPECT().Acknowledge(mock.Anything, "p", "s", "t").RunAndReturn(func(context.Context, string, string, string) error {
		log.add("ack")
		return nil
	}).Once()
	h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(testLogger(t)), httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(acker))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"process m#base", "process m#addon", "ack"}, log.get())
}

// A panic anywhere below the handler must become a 500 with a stack in the
// log, not a dropped connection (heroiclabs/nakama#2530).
func TestPanicsBecome500WithAStack(t *testing.T) {
	t.Parallel()
	ev := evFor("m1", "tok")
	ack := &google.Ack{PackageName: "p", SubscriptionID: "s", PurchaseToken: "t"}
	cases := map[string]func(t *testing.T, log *slog.Logger) http.Handler{
		"google parser": func(t *testing.T, log *slog.Logger) http.Handler {
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().ParseResult(mock.Anything, mock.Anything).RunAndReturn(
				func(context.Context, []byte) (google.Result, error) { panic("parser blew up") })
			return mustGoogle(t, parser, mocks.NewMockProcessor(t), log)
		},
		"google processor": func(t *testing.T, log *slog.Logger) http.Handler {
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().ParseResult(mock.Anything, mock.Anything).Return(gres(ev), nil)
			svc := mocks.NewMockProcessor(t)
			svc.EXPECT().Process(mock.Anything, ev).RunAndReturn(
				func(context.Context, entitlements.Event) (*entitlements.Entitlement, error) {
					panic("processor blew up")
				})
			return mustGoogle(t, parser, svc, log)
		},
		"google acknowledger": func(t *testing.T, log *slog.Logger) http.Handler {
			parser := mocks.NewMockGoogleParser(t)
			parser.EXPECT().ParseResult(mock.Anything, mock.Anything).Return(google.Result{Events: []entitlements.Event{ev}, Ack: ack}, nil)
			svc := mocks.NewMockProcessor(t)
			svc.EXPECT().Process(mock.Anything, ev).Return(granted, nil)
			acker := gmocks.NewMockAcknowledger(t)
			acker.EXPECT().Acknowledge(mock.Anything, "p", "s", "t").RunAndReturn(
				func(context.Context, string, string, string) error { panic("acknowledger blew up") })
			h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(log), httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(acker))
			require.NoError(t, err)
			return h
		},
		"apple parser": func(t *testing.T, log *slog.Logger) http.Handler {
			parser := mocks.NewMockAppleParser(t)
			parser.EXPECT().Parse(mock.Anything).RunAndReturn(func([]byte) (entitlements.Event, error) { panic("apple parser blew up") })
			return mustApple(t, parser, mocks.NewMockProcessor(t), httpx.WithLogger(log))
		},
		"apple processor": func(t *testing.T, log *slog.Logger) http.Handler {
			parser := mocks.NewMockAppleParser(t)
			parser.EXPECT().Parse(mock.Anything).Return(ev, nil)
			svc := mocks.NewMockProcessor(t)
			svc.EXPECT().Process(mock.Anything, ev).RunAndReturn(
				func(context.Context, entitlements.Event) (*entitlements.Entitlement, error) {
					panic("apple processor blew up")
				})
			return mustApple(t, parser, svc, httpx.WithLogger(log))
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log, logs := newRecorder()
			// A real server: net/http would close the connection on a panic.
			srv := httptest.NewServer(build(t, log))
			t.Cleanup(srv.Close)

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("body"))
			require.NoError(t, err)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err, "the connection must survive the panic")
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

			var found bool
			for _, r := range logs() {
				if r.Level == slog.LevelError && r.Msg == "panic while handling notification" {
					found = true
					assert.Contains(t, r.Attrs["panic"], "blew up")
					assert.Contains(t, r.Attrs["stack"], "goroutine")
				}
			}
			assert.True(t, found, "got %v", logs())

			// And it keeps serving.
			req2, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			require.NoError(t, err)
			resp2, err := srv.Client().Do(req2)
			require.NoError(t, err)
			require.NoError(t, resp2.Body.Close())
			assert.Equal(t, http.StatusMethodNotAllowed, resp2.StatusCode)
		})
	}
}
