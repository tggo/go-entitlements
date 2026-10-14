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
//
// Inbox mode (WithInbox) replaces all of the above after the request checks:
// the raw delivery is saved and answered 200, or 500 if saving failed.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/inbox"
)

var (
	// ErrAuthRequired is returned by NewGoogleHandler when neither a push
	// authenticator nor WithInsecureNoAuth is given.
	ErrAuthRequired = errors.New("httpx: push authentication is required (use WithPushAuthenticator, or WithInsecureNoAuth to opt out explicitly)")
	// ErrDependencyRequired is returned by a handler constructor for a nil
	// parser or processor.
	ErrDependencyRequired = errors.New("httpx: parser and processor are required")
	// ErrRouteRequired is returned by a handler constructor when WithInbox is
	// set without WithRoute.
	ErrRouteRequired = errors.New("httpx: WithInbox needs WithRoute (inbox.RouteProduction, RouteSandbox, ...)")
	// ErrRouteInvalid is returned when WithRoute is not one of the inbox.Route
	// constants.
	ErrRouteInvalid = errors.New("httpx: WithRoute is not an inbox.Route constant")
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
	inbox    inbox.Inbox
	onStored func(ctx context.Context, id string)
	route    inbox.Route
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

// WithInbox switches a handler to store-first mode: it verifies the request,
// saves the raw delivery to inbox, answers 200, and does nothing else. It does
// not parse and does not call the Processor, so the parser and processor
// arguments of the constructors may be nil; something else (a worker reading
// the inbox) does that later.
//
// Order: method, body limit and, for Google, push authentication run first,
// because a Pub/Sub OIDC token expires and cannot be verified later. Only then
// is the delivery saved. A Save error answers 500 so the sender retries; a
// success answers 200 (an empty or oversized body, which can never be stored,
// answers 400). Only the headers inbox.SafeHeaders allows reach the inbox:
// credentials such as Authorization never do.
//
// onStored, if not nil, is called synchronously after the 200 status is set,
// with the id Save returned and a context detached from the request (so it can
// outlive the response, for example to wake a worker). It must be quick. A
// panic in it is logged and never changes the answer. A nil inbox is ignored.
//
// With the App Store consumption wrapper (apple/serverapi.NewConsumptionHandler)
// the wrapper goes OUTSIDE this handler: it must see CONSUMPTION_REQUEST and
// answer it itself, and the inbox handler stores only what the wrapper
// forwards.
func WithInbox(in inbox.Inbox, onStored func(ctx context.Context, id string)) Option {
	return func(c *config) {
		if in != nil {
			c.inbox, c.onStored = in, onStored
		}
	}
}

// WithRoute sets the inbox.Route recorded with each raw delivery. It is
// required with WithInbox (the constructor returns ErrRouteRequired without it,
// ErrRouteInvalid for a value that is not one of the inbox.Route constants):
// there is no default, because a delivery filed under the wrong route is
// hard to spot later. Mount one handler per route, for example a production
// and a sandbox endpoint.
func WithRoute(route inbox.Route) Option { return func(c *config) { c.route = route } }

// checkInboxRoute validates the inbox-mode options.
func (c config) checkInboxRoute() error {
	if c.inbox == nil {
		return nil
	}
	switch c.route {
	case "":
		return ErrRouteRequired
	case inbox.RouteProduction, inbox.RouteSandbox, inbox.RouteHistory, inbox.RouteReconcile:
		return nil
	}
	return ErrRouteInvalid
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

// saveToInbox stores a verified raw delivery and answers. It is the whole
// request handling in inbox mode.
//
// rawID is the idempotency key for Save, or empty. It must only come from a
// value that is trustworthy at this point: an unverified Apple body must not
// choose its own id, or a forged body carrying a real notificationUUID would
// claim it and the genuine delivery would be dropped as a repeat.
func saveToInbox(w http.ResponseWriter, r *http.Request, cfg config, source entitlements.Source, rawID string, body []byte, log *slog.Logger) {
	route := cfg.route
	raw := inbox.Raw{
		ID:         rawID,
		Source:     source,
		Route:      route,
		Body:       body,
		Headers:    inbox.SafeHeaders(r.Header),
		ReceivedAt: time.Now().UTC(),
	}
	// Validate first so an item that can never be stored is rejected (400)
	// whatever the Inbox implementation does with it.
	var id string
	err := inbox.Validate(raw)
	if err == nil {
		id, err = cfg.inbox.Save(r.Context(), raw)
	}
	switch {
	case errors.Is(err, inbox.ErrInvalid):
		// An empty or oversized body can never be stored: retrying is pointless.
		log.Error("delivery cannot be stored, rejected", "error", err, "route", string(route))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	case err != nil:
		log.Error("inbox save failed, sender will retry", "error", err, "route", string(route))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Info("delivery stored", "inbox_id", id, "route", string(route), "bytes", len(body))
	w.WriteHeader(http.StatusOK)
	if cfg.onStored != nil {
		callOnStored(context.WithoutCancel(r.Context()), cfg.onStored, id, log)
	}
}

func callOnStored(ctx context.Context, fn func(context.Context, string), id string, log *slog.Logger) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("inbox onStored hook panicked", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()), "inbox_id", id)
		}
	}()
	fn(ctx, id)
}
