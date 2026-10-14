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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
)

var (
	// ErrAuthRequired is returned by NewGoogleHandler when neither a push
	// authenticator nor WithInsecureNoAuth is given.
	ErrAuthRequired = errors.New("httpx: push authentication is required (use WithPushAuthenticator, or WithInsecureNoAuth to opt out explicitly)")
	// ErrDependencyRequired is returned by a handler constructor for a nil
	// parser or processor.
	ErrDependencyRequired = errors.New("httpx: parser and processor are required")
)

// DefaultMaxBodyBytes is the request body limit when WithMaxBodyBytes is not set.
const DefaultMaxBodyBytes = 1 << 20

// Processor applies a normalized event. *entitlements.Service implements it.
type Processor interface {
	Process(ctx context.Context, ev entitlements.Event) (*entitlements.Entitlement, error)
}

type config struct {
	maxBody  int64
	auth     func(*http.Request) error
	log      *slog.Logger
	insecure bool
	acker    google.Acknowledger
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

// WithLogger sets the logger (default: discard). A nil logger keeps the
// default.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.log = l
		}
	}
}

// WithInsecureNoAuth lets NewGoogleHandler run without a push authenticator.
// Without authentication anyone who knows the endpoint can submit
// notifications: they cost Play API quota, create rows, and can feed the
// state machine forged data. Use it only for local demos or when something
// in front of the handler (a gateway that validates the Pub/Sub token)
// already authenticates the push. It logs an error when the handler is built.
func WithInsecureNoAuth() Option { return func(c *config) { c.insecure = true } }

// WithAcknowledger makes the Google handler acknowledge purchases that Google
// reports as pending, after the entitlement is stored. See NewGoogleHandler.
// A nil acknowledger is ignored.
func WithAcknowledger(a google.Acknowledger) Option {
	return func(c *config) {
		if a != nil {
			c.acker = a
		}
	}
}

func newConfig(opts []Option) config {
	c := config{maxBody: DefaultMaxBodyBytes, log: slog.New(slog.DiscardHandler)}
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

// process runs ev through p and logs the outcome once. It returns the HTTP
// status to answer and stored, which is true when the event is safely
// accounted for (applied, duplicate, stale, or a sandbox purchase the Service
// is configured to ignore) and false when nothing was stored.
func process(r *http.Request, p Processor, ev entitlements.Event, log *slog.Logger) (status int, stored bool) {
	ent, err := p.Process(r.Context(), ev)
	log = log.With("event", ev.ID, "source", string(ev.Source))
	switch {
	case err == nil:
		state := ""
		if ent != nil { // a Processor must not return (nil, nil), but do not panic if it does
			state = string(ent.State)
		}
		log.Info("notification applied", "state", state)
		return http.StatusOK, true
	case errors.Is(err, entitlements.ErrDuplicate):
		log.Info("duplicate notification")
		return http.StatusOK, true
	case errors.Is(err, entitlements.ErrStale):
		log.Info("stale notification, not applied")
		return http.StatusOK, true
	case errors.Is(err, entitlements.ErrIgnored):
		log.Info("notification ignored", "reason", err)
		return http.StatusOK, ev.Sandbox()
	case errors.Is(err, entitlements.ErrUnsupported):
		log.Warn("notification not applicable", "error", err)
		return http.StatusOK, false
	case errors.Is(err, entitlements.ErrInvalidEvent):
		// Permanent: the event can never be stored, so redelivery would loop.
		log.Error("invalid event dropped, acknowledged", "error", err)
		return http.StatusOK, false
	default:
		log.Error("process notification", "error", err)
		return http.StatusInternalServerError, false
	}
}

// respond writes the status chosen by process.
func respond(w http.ResponseWriter, status int) {
	if status == http.StatusOK {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, http.StatusText(status), status)
}

// recoverPanic turns a panic in a parser, processor, store or acknowledger
// into a 500 with an error log carrying the panic value and stack, instead of
// letting net/http drop the connection (which a push service retries as an
// unexplained failure). Use it as a deferred call at the top of ServeHTTP.
//
// A deterministic panic is a poison message: the push is redelivered until its
// retention ends. Configure a dead-letter topic and a maximum delivery count
// on the push subscription.
func recoverPanic(w http.ResponseWriter, log *slog.Logger) {
	rec := recover()
	if rec == nil {
		return
	}
	if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(rec) // net/http's own way to abort a response; not ours to swallow
	}
	log.Error("panic while handling notification", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
	http.Error(w, "internal error", http.StatusInternalServerError)
}
