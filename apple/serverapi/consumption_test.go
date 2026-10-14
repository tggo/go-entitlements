package serverapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func goodInfo() ConsumptionInfo {
	return ConsumptionInfo{CustomerConsented: true, DeliveryStatus: DeliveryDelivered, SampleContentProvided: true, ConsumptionPercentage: Percent(25), RefundPreference: RefundDecline}
}

func TestPercent(t *testing.T) {
	assert.Equal(t, 50000, *Percent(50))
	assert.Equal(t, 33333, *Percent(33.333))
	assert.Equal(t, 0, *Percent(0))
	assert.Equal(t, 100000, *Percent(100))
}

func TestConsumptionInfo_Validate(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		mod  func(*ConsumptionInfo)
		want error
	}{
		{"good", func(*ConsumptionInfo) {}, nil},
		{"no consent", func(c *ConsumptionInfo) { c.CustomerConsented = false }, ErrConsentRequired},
		{"no delivery status", func(c *ConsumptionInfo) { c.DeliveryStatus = "" }, ErrInvalidConsumptionInfo},
		{"unknown delivery status", func(c *ConsumptionInfo) { c.DeliveryStatus = "DELIVERED_LATE" }, ErrInvalidConsumptionInfo},
		{"unknown refund preference", func(c *ConsumptionInfo) { c.RefundPreference = "MAYBE" }, ErrInvalidConsumptionInfo},
		{"negative percentage", func(c *ConsumptionInfo) { c.ConsumptionPercentage = n(-1) }, ErrInvalidConsumptionInfo},
		{"over 100%", func(c *ConsumptionInfo) { c.ConsumptionPercentage = n(100001) }, ErrInvalidConsumptionInfo},
		{"100% is fine", func(c *ConsumptionInfo) { c.ConsumptionPercentage = n(100000) }, nil},
		{"undelivered with percentage", func(c *ConsumptionInfo) { c.DeliveryStatus = DeliveryServerOutage }, ErrInvalidConsumptionInfo},
		{"undelivered with zero", func(c *ConsumptionInfo) { c.DeliveryStatus = DeliveryServerOutage; c.ConsumptionPercentage = n(0) }, nil},
		{"undelivered without percentage", func(c *ConsumptionInfo) { c.DeliveryStatus = DeliveryWrongItem; c.ConsumptionPercentage = nil }, nil},
		{"prorated at 0", func(c *ConsumptionInfo) { c.RefundPreference = RefundGrantProrated; c.ConsumptionPercentage = n(0) }, ErrInvalidConsumptionInfo},
		{"prorated at 100%", func(c *ConsumptionInfo) {
			c.RefundPreference = RefundGrantProrated
			c.ConsumptionPercentage = n(100000)
		}, ErrInvalidConsumptionInfo},
		{"prorated in range", func(c *ConsumptionInfo) { c.RefundPreference = RefundGrantProrated; c.ConsumptionPercentage = n(1) }, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodInfo()
			tc.mod(&c)
			err := c.Validate()
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.want)
			assert.ErrorIs(t, err, ErrInvalidConsumptionInfo)
		})
	}
	assert.ErrorIs(t, ConsumptionInfo{}.Validate(), ErrConsentRequired, "the zero value never goes out")
}

func TestConsumptionInfo_ValidateFor(t *testing.T) {
	c := goodInfo()
	require.NoError(t, c.ValidateFor("Consumable"))
	require.NoError(t, c.ValidateFor(""))
	assert.ErrorIs(t, c.ValidateFor(autoRenewable), ErrInvalidConsumptionInfo, "a subscription takes no percentage")

	c.ConsumptionPercentage = nil
	c.RefundPreference = RefundGrantProrated
	require.NoError(t, c.ValidateFor(autoRenewable), "Apple computes a subscription's share")
	assert.ErrorIs(t, c.ValidateFor("Consumable"), ErrInvalidConsumptionInfo, "prorated consumable needs a percentage")
}

func TestSendConsumptionInfo_RequestShape(t *testing.T) {
	f := newFakeApple(t)
	f.respond(func(w http.ResponseWriter, _ recorded) { w.WriteHeader(http.StatusAccepted) })
	c, _ := f.client()

	require.NoError(t, c.SendConsumptionInfo(ctx, "2000000000000002", goodInfo()))
	r := f.requests()[0]
	assert.Equal(t, http.MethodPut, r.Method)
	assert.Equal(t, "/inApps/v2/transactions/consumption/2000000000000002", r.Path)
	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &body))
	assert.Equal(t, map[string]any{
		"customerConsented": true, "deliveryStatus": "DELIVERED", "sampleContentProvided": true,
		"consumptionPercentage": 25000.0, "refundPreference": "DECLINE",
	}, body)
}

func TestSendConsumptionInfo_RequiredFalsesAreSent(t *testing.T) {
	f := newFakeApple(t)
	f.respond(func(w http.ResponseWriter, _ recorded) { w.WriteHeader(http.StatusAccepted) })
	c, _ := f.client()
	info := ConsumptionInfo{CustomerConsented: true, DeliveryStatus: DeliveryDelivered}
	require.NoError(t, c.SendConsumptionInfo(ctx, "t", info))
	var body map[string]any
	require.NoError(t, json.Unmarshal(f.requests()[0].Body, &body))
	assert.Equal(t, false, body["sampleContentProvided"], "Apple requires the field even when false")
	assert.NotContains(t, body, "consumptionPercentage")
	assert.NotContains(t, body, "refundPreference")
}

func TestSendConsumptionInfo_UndeliveredGetsExplicitZero(t *testing.T) {
	f := newFakeApple(t)
	f.respond(func(w http.ResponseWriter, _ recorded) { w.WriteHeader(http.StatusAccepted) })
	c, _ := f.client()
	require.NoError(t, c.SendConsumptionInfo(ctx, "t", ConsumptionInfo{CustomerConsented: true, DeliveryStatus: DeliveryQualityIssue}))
	var body map[string]any
	require.NoError(t, json.Unmarshal(f.requests()[0].Body, &body))
	assert.Equal(t, 0.0, body["consumptionPercentage"])
}

func TestSendConsumptionInfo_NeverSendsWithoutConsentOrWithBadData(t *testing.T) {
	f := newFakeApple(t)
	c, _ := f.client()
	err := c.SendConsumptionInfo(ctx, "t", ConsumptionInfo{DeliveryStatus: DeliveryDelivered})
	assert.ErrorIs(t, err, ErrConsentRequired)
	err = c.SendConsumptionInfo(ctx, "t", ConsumptionInfo{CustomerConsented: true})
	assert.ErrorIs(t, err, ErrInvalidConsumptionInfo)
	assert.Error(t, c.SendConsumptionInfo(ctx, "", goodInfo()))
	assert.Empty(t, f.requests())
}

func TestSendConsumptionInfo_RetriesAndMapsErrors(t *testing.T) {
	f := newFakeApple(t)
	f.script(apiErr(500, 5000001, "again"), func(w http.ResponseWriter, _ recorded) { w.WriteHeader(http.StatusAccepted) })
	c, sl := f.client()
	require.NoError(t, c.SendConsumptionInfo(ctx, "t", goodInfo()))
	assert.Len(t, sl.all(), 1)

	f2 := newFakeApple(t)
	f2.respond(apiErr(404, 4040010, "Transaction id not found."))
	c2, _ := f2.client()
	assert.ErrorIs(t, c2.SendConsumptionInfo(ctx, "t", goodInfo()), ErrTransactionNotFound)
}

// --- notification parsing and the responder flow ---

func consumptionTx(over map[string]any) map[string]any {
	m := tx("2000000000000002", map[string]any{"appAccountToken": "7e3fb20b-4cdb-47cc-936d-99d65f608138"})
	for k, v := range over {
		m[k] = v
	}
	return m
}

func consumptionBody(t *testing.T, ca *testutil.AppleCA, typ string, txn map[string]any, reason string, signedMs int64) []byte {
	t.Helper()
	data := map[string]any{"bundleId": testBundle, "environment": "Sandbox", "signedTransactionInfo": ca.Sign(t, txn)}
	if reason != "" {
		data["consumptionRequestReason"] = reason
	}
	signed := ca.Sign(t, map[string]any{
		"notificationType": typ, "notificationUUID": "uuid-cr", "version": "2.0", "signedDate": signedMs, "data": data,
	})
	b, err := json.Marshal(map[string]string{"signedPayload": signed})
	require.NoError(t, err)
	return b
}

func TestParseConsumptionRequest(t *testing.T) {
	f := newFakeApple(t)
	v := f.verifier(entitlements.EnvironmentSandbox)
	body := consumptionBody(t, f.ca, "CONSUMPTION_REQUEST", consumptionTx(nil), "UNINTENDED_PURCHASE", 1791633600000)

	req, err := ParseConsumptionRequest(v, body)
	require.NoError(t, err)
	assert.Equal(t, "uuid-cr", req.NotificationUUID)
	assert.Equal(t, "2000000000000002", req.TransactionID)
	assert.Equal(t, origTx, req.OriginalTransactionID)
	assert.Equal(t, "7e3fb20b-4cdb-47cc-936d-99d65f608138", req.UserID)
	assert.Equal(t, "com.example.app.monthly", req.ProductID)
	assert.Equal(t, autoRenewable, req.ProductType)
	assert.Equal(t, entitlements.EnvironmentSandbox, req.Environment)
	assert.Equal(t, ReasonUnintendedPurchase, req.Reason)
	assert.Equal(t, snapshotAt, req.SignedAt)
	assert.Equal(t, snapshotAt.Add(12*time.Hour), req.Deadline)
	assert.Equal(t, "2000000000000002", req.Transaction.TransactionID)
}

func TestParseConsumptionRequest_Refusals(t *testing.T) {
	f := newFakeApple(t)
	v := f.verifier(entitlements.EnvironmentSandbox)
	_, err := ParseConsumptionRequest(v, consumptionBody(t, f.ca, "DID_RENEW", consumptionTx(nil), "", 1))
	assert.ErrorIs(t, err, ErrNotConsumptionRequest)
	assert.ErrorIs(t, err, entitlements.ErrIgnored)

	_, err = ParseConsumptionRequest(v, consumptionBody(t, testutil.NewAppleCA(t), "CONSUMPTION_REQUEST", consumptionTx(nil), "OTHER", 1))
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)

	_, err = ParseConsumptionRequest(v, consumptionBody(t, f.ca, "CONSUMPTION_REQUEST", consumptionTx(map[string]any{"bundleId": "com.evil"}), "OTHER", 1))
	assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)

	_, err = ParseConsumptionRequest(v, []byte("{"))
	assert.Error(t, err)
}

type fakeSender struct {
	calls []string
	infos []ConsumptionInfo
	err   error
}

func (s *fakeSender) SendConsumptionInfo(_ context.Context, id string, info ConsumptionInfo) error {
	s.calls = append(s.calls, id)
	s.infos = append(s.infos, info)
	return s.err
}

func respondWith(info ConsumptionInfo, err error, got *ConsumptionRequest) ConsumptionResponder {
	return ConsumptionResponderFunc(func(_ context.Context, req ConsumptionRequest) (ConsumptionInfo, error) {
		if got != nil {
			*got = req
		}
		return info, err
	})
}

func TestRespondToConsumptionRequest(t *testing.T) {
	req := ConsumptionRequest{TransactionID: "t2", ProductType: "Consumable", Reason: ReasonFulfillmentIssue}

	t.Run("sends the answer", func(t *testing.T) {
		s := &fakeSender{}
		var seen ConsumptionRequest
		out, err := RespondToConsumptionRequest(ctx, s, respondWith(goodInfo(), nil, &seen), req)
		require.NoError(t, err)
		assert.Equal(t, ConsumptionSent, out)
		assert.Equal(t, []string{"t2"}, s.calls, "the transactionId, not the original one")
		assert.Equal(t, ReasonFulfillmentIssue, seen.Reason)
	})
	t.Run("no consent is skipped, never sent", func(t *testing.T) {
		for name, r := range map[string]ConsumptionResponder{
			"zero info":          respondWith(ConsumptionInfo{}, nil, nil),
			"consent sentinel":   respondWith(ConsumptionInfo{}, ErrConsentRequired, nil),
			"wrapped sentinel":   respondWith(ConsumptionInfo{}, errors.Join(errors.New("x"), ErrConsentRequired), nil),
			"consented is false": respondWith(ConsumptionInfo{DeliveryStatus: DeliveryDelivered}, nil, nil),
		} {
			s := &fakeSender{}
			out, err := RespondToConsumptionRequest(ctx, s, r, req)
			require.NoError(t, err, name)
			assert.Equal(t, ConsumptionSkipped, out, name)
			assert.Empty(t, s.calls, name)
		}
	})
	t.Run("responder error", func(t *testing.T) {
		boom := errors.New("db down")
		s := &fakeSender{}
		_, err := RespondToConsumptionRequest(ctx, s, respondWith(ConsumptionInfo{}, boom, nil), req)
		assert.ErrorIs(t, err, boom)
		assert.Empty(t, s.calls)
	})
	t.Run("answer is checked against the product type", func(t *testing.T) {
		s := &fakeSender{}
		sub := ConsumptionRequest{TransactionID: "t2", ProductType: autoRenewable}
		_, err := RespondToConsumptionRequest(ctx, s, respondWith(goodInfo(), nil, nil), sub) // has a percentage
		assert.ErrorIs(t, err, ErrInvalidConsumptionInfo)
		assert.Empty(t, s.calls)
	})
	t.Run("send failure", func(t *testing.T) {
		s := &fakeSender{err: ErrServer}
		_, err := RespondToConsumptionRequest(ctx, s, respondWith(goodInfo(), nil, nil), req)
		assert.ErrorIs(t, err, ErrServer)
	})
}

type handlerRig struct {
	f      *fakeApple
	sender *fakeSender
	next   *int
	h      http.Handler
	now    time.Time
}

func newRig(t *testing.T, r ConsumptionResponder) *handlerRig {
	t.Helper()
	g := &handlerRig{f: newFakeApple(t), sender: &fakeSender{}, next: new(int), now: snapshotAt.Add(time.Minute)}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*g.next++
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		w.Header().Set("X-Next-Body-Len", string(rune('0'+len(b.Bytes())%10)))
		w.WriteHeader(http.StatusTeapot)
	})
	g.h = NewConsumptionHandler(next, ConsumptionHandlerConfig{
		Verifier: g.f.verifier(entitlements.EnvironmentSandbox), Responder: r, Sender: g.sender,
		Clock: func() time.Time { return g.now },
	})
	return g
}

func (g *handlerRig) post(body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/apple", bytes.NewReader(body)))
	return rec
}

func (g *handlerRig) crBody(t *testing.T) []byte {
	return consumptionBody(t, g.f.ca, "CONSUMPTION_REQUEST", consumptionTx(map[string]any{"type": "Consumable"}), "UNSATISFIED_WITH_PURCHASE", snapshotAt.UnixMilli())
}

func TestConsumptionHandler_AnswersAndAcknowledges(t *testing.T) {
	g := newRig(t, respondWith(goodInfo(), nil, nil))
	rec := g.post(g.crBody(t))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"2000000000000002"}, g.sender.calls)
	assert.Zero(t, *g.next, "the wrapped handler is not involved")
}

func TestConsumptionHandler_NoConsentSendsNothing(t *testing.T) {
	g := newRig(t, respondWith(ConsumptionInfo{}, ErrConsentRequired, nil))
	assert.Equal(t, http.StatusOK, g.post(g.crBody(t)).Code)
	assert.Empty(t, g.sender.calls)
}

func TestConsumptionHandler_FailureInsideDeadlineAsksForRedelivery(t *testing.T) {
	g := newRig(t, respondWith(ConsumptionInfo{}, errors.New("db down"), nil))
	assert.Equal(t, http.StatusInternalServerError, g.post(g.crBody(t)).Code)

	g2 := newRig(t, respondWith(goodInfo(), nil, nil))
	g2.sender.err = ErrServer
	assert.Equal(t, http.StatusInternalServerError, g2.post(g2.crBody(t)).Code)
}

func TestConsumptionHandler_FailureAfterDeadlineIsNotRetried(t *testing.T) {
	g := newRig(t, respondWith(ConsumptionInfo{}, errors.New("db down"), nil))
	g.now = snapshotAt.Add(12 * time.Hour)
	assert.Equal(t, http.StatusOK, g.post(g.crBody(t)).Code, "the deadline is exclusive: a redelivery cannot help")
	g.now = snapshotAt.Add(12*time.Hour - time.Second)
	assert.Equal(t, http.StatusInternalServerError, g.post(g.crBody(t)).Code)
}

func TestConsumptionHandler_ResponderMistakeIsNotRetried(t *testing.T) {
	bad := goodInfo()
	bad.DeliveryStatus = "NOPE"
	g := newRig(t, respondWith(bad, nil, nil))
	assert.Equal(t, http.StatusOK, g.post(g.crBody(t)).Code, "the same wrong answer would come again")
	assert.Empty(t, g.sender.calls)
}

func TestConsumptionHandler_RejectsForgeriesAndGarbage(t *testing.T) {
	g := newRig(t, respondWith(goodInfo(), nil, nil))
	rogue := consumptionBody(t, testutil.NewAppleCA(t), "CONSUMPTION_REQUEST", consumptionTx(nil), "OTHER", snapshotAt.UnixMilli())
	assert.Equal(t, http.StatusUnauthorized, g.post(rogue).Code)
	assert.Empty(t, g.sender.calls, "an unverified request never reaches the responder")

	other := consumptionTx(map[string]any{"bundleId": "com.evil.app"})
	assert.Equal(t, http.StatusUnauthorized, g.post(consumptionBody(t, g.f.ca, "CONSUMPTION_REQUEST", other, "OTHER", 1)).Code)
}

func TestConsumptionHandler_OtherRequestsPassThroughIntact(t *testing.T) {
	g := newRig(t, respondWith(goodInfo(), nil, nil))
	renew := consumptionBody(t, g.f.ca, "DID_RENEW", consumptionTx(nil), "", 1)

	rec := g.post(renew)
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, 1, *g.next)
	assert.Equal(t, string(rune('0'+len(renew)%10)), rec.Header().Get("X-Next-Body-Len"), "next still reads the whole body")

	for _, body := range [][]byte{[]byte("not json"), []byte(`{"signedPayload":"a.b.c"}`), []byte(`{}`)} {
		assert.Equal(t, http.StatusTeapot, g.post(body).Code, "anything unclassifiable is next's to reject")
	}

	rec = httptest.NewRecorder()
	g.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apple", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Empty(t, g.sender.calls)
}

func TestConsumptionHandler_BodyLimit(t *testing.T) {
	g := newRig(t, respondWith(goodInfo(), nil, nil))
	h := NewConsumptionHandler(http.NotFoundHandler(), ConsumptionHandlerConfig{
		Verifier: g.f.verifier(entitlements.EnvironmentSandbox), Responder: respondWith(goodInfo(), nil, nil), Sender: g.sender, MaxBody: 10,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(make([]byte, 100))))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestConsumptionHandler_ForgedTypeClaimCannotSkipVerification(t *testing.T) {
	// A body whose unverified type says DID_RENEW goes to next (which
	// verifies); one that says CONSUMPTION_REQUEST is verified here.
	g := newRig(t, respondWith(goodInfo(), nil, nil))
	assert.Equal(t, "CONSUMPTION_REQUEST", peekType(g.crBody(t)))
	assert.Equal(t, "", peekType([]byte("garbage")))
	assert.Equal(t, "", peekType([]byte(`{"signedPayload":"a.!!.c"}`)))
	assert.Equal(t, "", peekType([]byte(`{"signedPayload":"a.e30.c"}`)))
}
