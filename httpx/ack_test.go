package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
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

var (
	ackEvent = entitlements.Event{ID: "m1", Source: entitlements.SourceGoogle, OriginalTxID: "tok-1", State: entitlements.StateActive}
	ackReq   = &google.Ack{PackageName: "com.example.app", SubscriptionID: "premium.monthly", PurchaseToken: "tok-1"}
	granted  = &entitlements.Entitlement{State: entitlements.StateActive}
)

// steps records the sequence of Process and Acknowledge calls.
type steps struct {
	mu sync.Mutex
	s  []string
}

func (o *steps) add(v string) { o.mu.Lock(); o.s = append(o.s, v); o.mu.Unlock() }
func (o *steps) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.s...)
}

// ackHandler wires Parse -> res, Process -> (granted, procErr) and, when ack is
// non-nil, Acknowledge -> ackErr. It returns the handler and the call log.
func ackHandler(t *testing.T, res google.Result, procErr, ackErr error, expectAck bool) (http.Handler, *steps) {
	t.Helper()
	log := &steps{}
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(res, nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, res.Events[0]).RunAndReturn(
		func(context.Context, entitlements.Event) (*entitlements.Entitlement, error) {
			log.add("process")
			return granted, procErr
		})
	acker := gmocks.NewMockAcknowledger(t)
	if expectAck {
		acker.EXPECT().Acknowledge(mock.Anything, ackReq.PackageName, ackReq.SubscriptionID, ackReq.PurchaseToken).RunAndReturn(
			func(context.Context, string, string, string) error {
				log.add("ack")
				return ackErr
			})
	}
	h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(testLogger(t)), httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(acker))
	require.NoError(t, err)
	return h, log
}

func TestAck_AfterProcessAndOnlyThen(t *testing.T) {
	t.Parallel()
	h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ackEvent}, Ack: ackReq}, nil, nil, true)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"process", "ack"}, log.get(), "the entitlement is stored before the purchase is acknowledged")
}

func TestAck_NothingToAcknowledge(t *testing.T) {
	t.Parallel()
	h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ackEvent}}, nil, nil, false)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"process"}, log.get())
}

func TestAck_StillAttemptedOnDuplicateAndStale(t *testing.T) {
	t.Parallel()
	for name, procErr := range map[string]error{
		"duplicate (redelivery after a failed ack)": fmt.Errorf("w: %w", entitlements.ErrDuplicate),
		"stale": fmt.Errorf("w: %w", entitlements.ErrStale),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ackEvent}, Ack: ackReq}, procErr, nil, true)

			assert.Equal(t, http.StatusOK, post(h, "body").Code)
			assert.Equal(t, []string{"process", "ack"}, log.get())
		})
	}
}

func TestAck_NeverWhenTheEntitlementWasNotStored(t *testing.T) {
	t.Parallel()
	for name, procErr := range map[string]error{
		"store failure":     errors.New("db down"),
		"invalid event":     fmt.Errorf("w: %w", entitlements.ErrInvalidEvent),
		"ignored":           fmt.Errorf("w: %w", entitlements.ErrIgnored),
		"unsupported event": fmt.Errorf("w: %w", entitlements.ErrUnsupported),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ackEvent}, Ack: ackReq}, procErr, nil, false)

			post(h, "body")
			assert.Equal(t, []string{"process"}, log.get())
		})
	}
}

// A license-tester purchase is ignored by a Service without WithAllowSandbox,
// but Google would still refund it after 3 days, so it is acknowledged.
func TestAck_SandboxPurchaseIgnoredByServiceIsStillAcknowledged(t *testing.T) {
	t.Parallel()
	ev := ackEvent
	ev.Environment = entitlements.EnvironmentGoogleTest
	h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ev}, Ack: ackReq}, fmt.Errorf("w: %w", entitlements.ErrIgnored), nil, true)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"process", "ack"}, log.get())
}

func TestAck_FailureClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"already acknowledged", fmt.Errorf("w: %w", google.ErrAlreadyAcknowledged), http.StatusOK},
		{"purchase not found", fmt.Errorf("w: %w", google.ErrPurchaseNotFound), http.StatusOK},
		{"purchase invalid", fmt.Errorf("w: %w", google.ErrPurchaseInvalid), http.StatusOK},
		{"config", fmt.Errorf("w: %w", google.ErrFetchConfig), http.StatusInternalServerError},
		{"transient", fmt.Errorf("w: %w", google.ErrFetchTransient), http.StatusInternalServerError},
		{"unclassified", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, log := ackHandler(t, google.Result{Events: []entitlements.Event{ackEvent}, Ack: ackReq}, nil, tc.err, true)

			assert.Equal(t, tc.code, post(h, "body").Code)
			assert.Equal(t, []string{"process", "ack"}, log.get())
		})
	}
}

// First delivery: Process stores, Acknowledge fails transiently -> 500.
// Pub/Sub redelivers: Process says ErrDuplicate, Acknowledge is tried again
// and succeeds -> 200.
func TestAck_RedeliveryAfterFailedAckAcknowledgesAgain(t *testing.T) {
	t.Parallel()
	log := &steps{}
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(google.Result{Events: []entitlements.Event{ackEvent}, Ack: ackReq}, nil).Twice()
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, ackEvent).Return(granted, nil).Once()
	svc.EXPECT().Process(mock.Anything, ackEvent).Return(nil, entitlements.ErrDuplicate).Once()
	acker := gmocks.NewMockAcknowledger(t)
	acker.EXPECT().Acknowledge(mock.Anything, ackReq.PackageName, ackReq.SubscriptionID, ackReq.PurchaseToken).
		RunAndReturn(func(context.Context, string, string, string) error {
			log.add("ack")
			if len(log.get()) == 1 {
				return google.ErrFetchTransient
			}
			return nil
		}).Twice()
	h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(testLogger(t)), httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(acker))
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, post(h, "body").Code)
	assert.Equal(t, http.StatusOK, post(h, "body").Code)
	assert.Equal(t, []string{"ack", "ack"}, log.get())
}

func TestAck_NilAcknowledgerOptionIsIgnored(t *testing.T) {
	t.Parallel()
	parser := mocks.NewMockGoogleParser(t)
	parser.EXPECT().ParseResult(mock.Anything, []byte("body")).Return(gres(ackEvent), nil)
	svc := mocks.NewMockProcessor(t)
	svc.EXPECT().Process(mock.Anything, ackEvent).Return(granted, nil)
	h, err := httpx.NewGoogleHandler(parser, svc, httpx.WithInsecureNoAuth(), httpx.WithAcknowledger(nil))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, post(h, "body").Code)
}
