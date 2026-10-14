package serverapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

const defaultMaxBody = 1 << 20

// ConsumptionHandlerConfig configures NewConsumptionHandler.
type ConsumptionHandlerConfig struct {
	// Verifier verifies the notification (pin your bundle ID and environment).
	Verifier *apple.Verifier
	// Responder answers refund requests.
	Responder ConsumptionResponder
	// Sender sends the answer; pass the *Client.
	Sender ConsumptionSender
	// Logger defaults to discarding.
	Logger *slog.Logger
	// Clock defaults to time.Now; used for the deadline.
	Clock func() time.Time
	// MaxBody limits the request body (default 1 MiB).
	MaxBody int64
}

type consumptionHandler struct {
	next http.Handler
	cfg  ConsumptionHandlerConfig
	log  *slog.Logger
	now  func() time.Time
	max  int64
}

// NewConsumptionHandler wraps the App Store notification handler (httpx's
// NewAppleHandler) and answers CONSUMPTION_REQUEST itself. Everything else,
// including any request it cannot classify, goes to next unchanged, so
// wrapping needs no change to the handler it wraps:
//
//	mux.Handle("/apple", serverapi.NewConsumptionHandler(httpx.NewAppleHandler(parser, svc), cfg))
//
// Routing peeks at the notificationType without trusting it; a request that
// claims to be CONSUMPTION_REQUEST is then fully verified here (a forged one
// gets 401), and one that lies about being something else reaches next, which
// verifies it.
//
// Status codes for a CONSUMPTION_REQUEST: 200 when the answer was sent, when
// the customer did not consent (nothing to send), or when the 12-hour
// deadline has passed (a retry could not help); 500 when the responder or
// Apple failed inside the deadline, so the App Store redelivers; 401 for a bad
// signature; 400 for a malformed body.
//
// Redelivery: for version 2 notifications the production App Store retries
// after a non-2xx at 1, 12, 24, 48 and 72 hours after the previous attempt
// (https://developer.apple.com/documentation/appstoreservernotifications/responding-to-app-store-server-notifications),
// the Sandbox does not retry. Against the 12-hour deadline only the first
// retry, one hour later, can still count. The Client itself already retries
// 429 and 5xx answers for a few seconds before the handler gives up. Keep the responder
// fast and idempotent.
func NewConsumptionHandler(next http.Handler, cfg ConsumptionHandlerConfig) http.Handler {
	h := &consumptionHandler{next: next, cfg: cfg, log: cfg.Logger, now: cfg.Clock, max: cfg.MaxBody}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.max <= 0 {
		h.max = defaultMaxBody
	}
	return h
}

func (h *consumptionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if peekType(body) != "CONSUMPTION_REQUEST" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.next.ServeHTTP(w, r)
		return
	}
	req, err := ParseConsumptionRequest(h.cfg.Verifier, body)
	switch {
	case errors.Is(err, entitlements.ErrInvalidSignature):
		h.log.Error("consumption request: invalid signature", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	case err != nil:
		h.log.Error("consumption request: malformed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(h.answer(r.Context(), req))
}

func (h *consumptionHandler) answer(ctx context.Context, req ConsumptionRequest) int {
	out, err := RespondToConsumptionRequest(ctx, h.cfg.Sender, h.cfg.Responder, req)
	if err == nil {
		h.log.Info("consumption request answered", "notification", req.NotificationUUID, "outcome", out)
		return http.StatusOK
	}
	if !h.now().Before(req.Deadline) {
		h.log.Error("consumption request failed after the 12-hour deadline; giving up", "notification", req.NotificationUUID, "error", err)
		return http.StatusOK
	}
	if errors.Is(err, ErrInvalidConsumptionInfo) {
		// The responder's answer is wrong; redelivery would repeat it.
		h.log.Error("consumption request: responder returned an invalid answer", "notification", req.NotificationUUID, "error", err)
		return http.StatusOK
	}
	h.log.Error("consumption request failed; asking Apple to redeliver", "notification", req.NotificationUUID, "error", err)
	return http.StatusInternalServerError
}

// signedPayloadOf extracts the signedPayload JWS from a notification body.
func signedPayloadOf(body []byte) (string, error) {
	var rb apple.ResponseBodyV2
	if err := json.Unmarshal(body, &rb); err != nil {
		return "", err
	}
	if rb.SignedPayload == "" {
		return "", errors.New("serverapi: empty signedPayload")
	}
	return rb.SignedPayload, nil
}

// peekType reads the notificationType of a body WITHOUT verifying it. It is
// only a routing hint; every path that acts on the notification verifies it.
func peekType(body []byte) string {
	sp, err := signedPayloadOf(body)
	if err != nil {
		return ""
	}
	parts := strings.Split(sp, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var p struct {
		NotificationType string `json:"notificationType"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.NotificationType
}
