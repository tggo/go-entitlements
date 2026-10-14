package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
)

// GoogleParser decodes an RTDN Pub/Sub push body and fetches the real
// subscription state. It returns every event the push implies (one per line
// item of a subscription with add-ons) and the acknowledgement the purchase
// needs. *google.Parser implements it.
type GoogleParser interface {
	ParseResult(ctx context.Context, body []byte) (google.Result, error)
}

type googleHandler struct {
	parser GoogleParser
	svc    Processor
	log    *slog.Logger
	cfg    config
}

// NewGoogleHandler returns the handler for the Pub/Sub push endpoint that
// receives Google Play RTDN.
//
// With WithAcknowledger the handler also acknowledges purchases Google reports
// as pending, because Google refunds unacknowledged purchases after 3 days.
// The order is fixed: the event is processed first, and only when it is stored
// (applied, duplicate or stale) is the purchase acknowledged. An acknowledge
// failure that a retry can fix (5xx, 429, timeout) answers 500 so Pub/Sub
// redelivers; the redelivered event is a duplicate, which still triggers the
// acknowledge again, so a lost acknowledge is retried until it succeeds. A
// purchase that is already acknowledged, unknown or invalid is logged and
// answered 200. A 401/403 is a configuration error: 500 with an error log.
//
// Push authentication is mandatory: pass WithPushAuthenticator (see
// httpx/pubsubauth) or, deliberately, WithInsecureNoAuth; otherwise the
// constructor returns an error. A failed authenticator answers 401.
//
// Malformed pushes answer 400. A notification for another package name, a
// purchase token the Play API reports as 400/404/410, or a granting
// subscription without expiryTime is acknowledged (200, logged): redelivery
// cannot fix it. A 401/403 from the Play API is a configuration error: 500
// with an error log that says so. Any other fetch failure (5xx, 429,
// timeouts) answers 500 so Pub/Sub redelivers.
func NewGoogleHandler(parser GoogleParser, svc Processor, opts ...Option) (http.Handler, error) {
	if parser == nil || svc == nil {
		return nil, ErrDependencyRequired
	}
	cfg := newConfig(opts)
	log := cfg.log.With("handler", "google")
	if cfg.auth == nil {
		if !cfg.insecure {
			return nil, ErrAuthRequired
		}
		log.Error("push authentication disabled", "risk", "anyone who can reach this endpoint can submit forged notifications")
	}
	return &googleHandler{parser: parser, svc: svc, log: log, cfg: cfg}, nil
}

func (h *googleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer recoverPanic(w, h.log)
	if r.Method == http.MethodPost && h.cfg.auth != nil {
		if err := h.cfg.auth(r); err != nil {
			h.log.Error("rejected push: authentication failed", "error", err, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	body, ok := readBody(w, r, h.cfg.maxBody, h.log)
	if !ok {
		return
	}
	res, err := h.parser.ParseResult(r.Context(), body)
	switch {
	case err == nil:
		status := h.processAll(r, res)
		respond(w, status)
	case errors.Is(err, google.ErrNoExpiry):
		// A granting event was dropped; the entitlement stays as it was until
		// the next notification repairs it.
		h.log.Error("granting subscription without expiryTime dropped, acknowledged", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, google.ErrTestNotification):
		h.log.Debug("play console test notification")
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, entitlements.ErrIgnored):
		h.log.Info("notification ignored", "reason", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, entitlements.ErrUnsupported):
		h.log.Warn("notification not applicable", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, google.ErrPackageMismatch):
		h.log.Warn("notification for another package, acknowledged", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, google.ErrPurchaseInvalid):
		h.log.Error("purchase token rejected as invalid by the play api, acknowledged", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, google.ErrPurchaseNotFound):
		h.log.Warn("purchase token unknown or expired", "error", err, "action", "acknowledged")
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, google.ErrFetchConfig):
		h.log.Error("play api credentials or permissions rejected", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	case errors.Is(err, google.ErrFetchTransient):
		h.log.Error("play api transient failure", "error", err, "action", "pubsub will redeliver")
		http.Error(w, "internal error", http.StatusInternalServerError)
	case errors.Is(err, google.ErrMalformed):
		h.log.Error("rejected push: malformed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "bad request", http.StatusBadRequest)
	case errors.Is(err, entitlements.ErrInvalidSignature):
		h.log.Error("rejected push: invalid signature", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	default:
		h.log.Error("parse push", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// acknowledge acknowledges ack and returns the status to answer.
func (h *googleHandler) acknowledge(ctx context.Context, ev entitlements.Event, ack *google.Ack) int {
	err := h.cfg.acker.Acknowledge(ctx, ack.PackageName, ack.SubscriptionID, ack.PurchaseToken)
	log := h.log.With("event", ev.ID)
	switch {
	case err == nil:
		log.Info("purchase acknowledged", "subscription", ack.SubscriptionID)
	case errors.Is(err, google.ErrAlreadyAcknowledged):
		log.Info("purchase already acknowledged")
	case errors.Is(err, google.ErrPurchaseNotFound), errors.Is(err, google.ErrPurchaseInvalid):
		log.Error("purchase cannot be acknowledged, acknowledged push", "error", err)
	case errors.Is(err, google.ErrFetchConfig):
		log.Error("play api credentials or permissions rejected the acknowledge", "error", err)
		return http.StatusInternalServerError
	default:
		log.Error("acknowledge failed, pubsub will redeliver", "error", err)
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

// processAll applies every event of the push in order and then, if every one
// is stored and the purchase is pending, acknowledges it. The answer is 500 if
// any event hit a retryable failure (the whole push is redelivered; events
// already stored come back as duplicates), otherwise 200. A failing event does
// not stop the others: each is independent.
func (h *googleHandler) processAll(r *http.Request, res google.Result) int {
	status, allStored := http.StatusOK, true
	for _, ev := range res.Events {
		st, stored := process(r, h.svc, ev, h.log)
		if st != http.StatusOK {
			status = st
		}
		allStored = allStored && stored
	}
	if status == http.StatusOK && allStored && res.Ack != nil && h.cfg.acker != nil && len(res.Events) > 0 {
		status = h.acknowledge(r.Context(), res.Events[0], res.Ack)
	}
	return status
}
