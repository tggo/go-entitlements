// Package httpx provides net/http handlers that receive App Store Server
// Notifications V2 and Google Play RTDN Pub/Sub pushes and feed them to an
// entitlements.Service.
//
// Handlers are safe for concurrent use if their Processor and parser are.
// Loggers are *slog.Logger; bridge zap with zapslog.
//
// Response policy (both handlers):
//
//   - 200: applied, ErrDuplicate, ErrStale, ErrIgnored, ErrUnsupported, and
//     ErrInvalidEvent (a permanently unusable event; retrying cannot help, so
//     it is acknowledged and logged at error level). The store should stop
//     retrying; the outcome is logged.
//   - 400/401: the request can never succeed (bad signature, malformed body,
//     failed push authentication). Retrying does not help; the failure is
//     logged at error level because it is either an attack or a bug.
//   - 405: not a POST. 413: body over the limit.
//   - 500: a transient failure (store, network fetch). The store retries.
package httpx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/tggo/go-entitlements"
)

// DefaultMaxBodyBytes is the request body limit when WithMaxBodyBytes is not set.
const DefaultMaxBodyBytes = 1 << 20

// Processor applies a normalized event. *entitlements.Service implements it.
type Processor interface {
	Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error)
}

type config struct {
	maxBody int64
	auth    func(*http.Request) error
}

// Option configures a handler.
type Option func(*config)

// WithMaxBodyBytes sets the request body limit (default DefaultMaxBodyBytes).
func WithMaxBodyBytes(n int64) Option { return func(c *config) { c.maxBody = n } }

// WithPushAuthenticator sets a check that runs before the body is read; a
// non-nil error answers 401. It applies to the Google handler only (Apple
// requests are authenticated by the JWS signature itself). See package
// httpx/pubsubauth for the Pub/Sub OIDC check.
func WithPushAuthenticator(fn func(*http.Request) error) Option {
	return func(c *config) { c.auth = fn }
}

func newConfig(opts []Option) config {
	c := config{maxBody: DefaultMaxBodyBytes}
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

// readBody enforces POST and the size limit. It writes the error response
// itself and returns ok=false when the request must not proceed.
func readBody(w http.ResponseWriter, r *http.Request, limit int64, log *slog.Logger) ([]byte, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			log.Warn("request body too large", "limit", tooBig.Limit)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		log.Warn("read request body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// process runs ev through p and answers per the response policy.
func process(w http.ResponseWriter, r *http.Request, p Processor, ev entitlements.Event, log *slog.Logger) {
	ent, err := p.Process(r.Context(), ev)
	log = log.With("event", ev.ID, "source", string(ev.Source))
	switch {
	case err == nil:
		state := ""
		if ent != nil { // a Processor must not return (nil, nil), but do not panic if it does
			state = string(ent.State)
		}
		log.Info("notification applied", "state", state)
	case errors.Is(err, entitlements.ErrDuplicate):
		log.Info("duplicate notification")
	case errors.Is(err, entitlements.ErrStale):
		log.Info("stale notification, not applied")
	case errors.Is(err, entitlements.ErrIgnored), errors.Is(err, entitlements.ErrUnsupported):
		log.Warn("notification not applicable", "error", err)
	case errors.Is(err, entitlements.ErrInvalidEvent):
		// Permanent: the event can never be stored, so redelivery would loop.
		log.Error("invalid event dropped, acknowledged", "error", err)
	default:
		log.Error("process notification", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
