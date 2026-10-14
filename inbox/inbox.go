// Package inbox is an optional "store first, process later" layer in front of
// the webhook handlers.
//
// A handler that saves the raw request body to an Inbox before doing anything
// else and answers 200 as soon as that write succeeds can no longer lose a
// notification to a verification bug, a store outage or a deploy: the body is
// on disk. A worker (a goroutine, a Temporal activity, a cron job) then calls
// ProcessStored for each saved item, and the same function replays items later.
//
// Default behaviour of the library is unchanged: nothing here is used unless
// you pass an Inbox to the handler (httpx.WithInbox) or call ProcessStored.
package inbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tggo/go-entitlements"
)

// Route says which endpoint or job produced an item. It is metadata for
// filtering and audit; processing does not depend on it (the verifier's own
// environment pin decides what is accepted).
type Route string

const (
	// RouteProduction is a notification received on the production endpoint.
	RouteProduction Route = "prod"
	// RouteSandbox is a notification received on the sandbox endpoint.
	RouteSandbox Route = "sandbox"
	// RouteHistory is a notification fetched from a store's history API.
	RouteHistory Route = "history"
	// RouteReconcile is a synthetic notification produced by a reconciliation job.
	RouteReconcile Route = "reconcile"
)

// Status is where an item is in its life.
//
// Retryable statuses (Received, Failed) mean processing can still succeed;
// ProcessStored returns an error with them so a worker retries. Every other
// status is final: the outcome will not change by trying again.
type Status string

const (
	// StatusReceived: saved, not processed yet.
	StatusReceived Status = "received"
	// StatusProcessed: verified and applied to the entitlement.
	StatusProcessed Status = "processed"
	// StatusDuplicate: every event in it was already applied before.
	StatusDuplicate Status = "duplicate"
	// StatusStale: a newer event had already been applied.
	StatusStale Status = "stale"
	// StatusIgnored: carries no subscription state (TEST, one-time charge, a
	// notification for another app).
	StatusIgnored Status = "ignored"
	// StatusUnsupported: a type or state the library does not map.
	StatusUnsupported Status = "unsupported"
	// StatusInvalidSignature: verification failed or the body is malformed;
	// retrying cannot help.
	StatusInvalidSignature Status = "invalid_signature"
	// StatusFailed: the last attempt hit a transient error (store or Play API
	// outage). Retryable.
	StatusFailed Status = "failed"
)

// Final reports whether the status will not change by processing again.
func (s Status) Final() bool { return s != StatusReceived && s != StatusFailed }

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusReceived, StatusProcessed, StatusDuplicate, StatusStale, StatusIgnored,
		StatusUnsupported, StatusInvalidSignature, StatusFailed:
		return true
	}
	return false
}

// Raw is what a handler saves: the request exactly as received.
type Raw struct {
	// ID is optional. Set it to something unique per delivery to make Save
	// idempotent: saving the same ID again returns it without adding a row.
	// Empty means the Inbox generates one.
	//
	// Use a Google Pub/Sub messageId only after the push token was checked.
	// Leave it empty for Apple: the notificationUUID in an unverified body can
	// be claimed by a forged request, and the genuine notification would then
	// be dropped as already saved.
	ID string
	// Source is SourceApple or SourceGoogle and selects the parser.
	Source entitlements.Source
	Route  Route
	// Body is the request body, unmodified (for Apple the {"signedPayload":…}
	// JSON; for Google the Pub/Sub push envelope).
	Body []byte
	// Headers are the request headers worth keeping. Save drops credentials
	// (see SafeHeaders), whatever the caller passes.
	Headers map[string][]string
	// ReceivedAt is when the handler got the request (zero: the Inbox stamps now).
	ReceivedAt time.Time
}

// Stored is an item as an Inbox holds it.
type Stored struct {
	Raw
	ID        string // always set
	Status    Status
	Error     string // last error text; empty on success
	Attempts  int    // how many times processing ran
	UpdatedAt time.Time
}

// Filter selects items for List. Zero fields match everything.
type Filter struct {
	Status Status
	Source entitlements.Source
	// Since keeps items received at or after it.
	Since time.Time
	// Limit caps the page; 0 means 100.
	Limit int
	// After is the ID of the last item of the previous page. Items are ordered
	// by (ReceivedAt, ID) ascending, so paging with After visits each item once.
	After string
}

// DefaultListLimit is the page size when Filter.Limit is 0.
const DefaultListLimit = 100

// MaxBodyBytes is the largest body an Inbox accepts; handlers already cap
// requests (1 MiB by default), this is the backstop.
const MaxBodyBytes = 4 << 20

// Inbox stores raw notifications until they are processed. Implementations
// are safe for concurrent use and must pass inboxtest.Run.
//
// Contract:
//  1. Save returns the item's ID. It stamps ReceivedAt when zero, sets the
//     status to StatusReceived, attempts to 0, and keeps only SafeHeaders.
//     A Raw with an ID that already exists returns that ID without changing
//     the stored item. A body over MaxBodyBytes or an empty body returns
//     ErrInvalid.
//  2. Get returns a copy, or entitlements.ErrNotFound.
//  3. Mark sets status, error text and attempts. It is idempotent (marking
//     the same values twice is fine) and returns entitlements.ErrNotFound for
//     an unknown ID. It refuses an invalid Status with ErrInvalid.
//  4. List returns items ordered by (ReceivedAt, ID) ascending, honouring
//     every Filter field; After continues after that item.
//  5. Prune deletes final items received before the cutoff and returns how
//     many; retryable items are never pruned.
//  6. Returned values never alias stored ones.
type Inbox interface {
	Save(ctx context.Context, raw Raw) (id string, err error)
	Get(ctx context.Context, id string) (*Stored, error)
	Mark(ctx context.Context, id string, status Status, errText string, attempts int) error
	List(ctx context.Context, f Filter) ([]Stored, error)
	Prune(ctx context.Context, before time.Time) (int64, error)
}

// ErrInvalid means the item or argument cannot be stored (empty or oversized
// body, unknown source or status).
var ErrInvalid = errors.New("inbox: invalid item")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// safeHeaders is the allowlist of request headers an Inbox keeps. Credentials
// (Authorization carries the Pub/Sub OIDC token) must never be stored.
var safeHeaders = map[string]bool{
	"content-type": true, "content-length": true, "user-agent": true, "x-request-id": true,
	"x-forwarded-for": true, "x-cloud-trace-context": true, "traceparent": true,
}

// SafeHeaders returns a copy of h reduced to the allowlisted, non-credential
// headers, with canonical names. Inbox implementations call it in Save.
func SafeHeaders(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := map[string][]string{}
	for k, v := range h {
		if safeHeaders[strings.ToLower(k)] {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Validate checks a Raw before it is stored.
func Validate(raw Raw) error {
	switch {
	case len(raw.Body) == 0:
		return invalidf("empty body")
	case len(raw.Body) > MaxBodyBytes:
		return invalidf("body over %d bytes", MaxBodyBytes)
	case raw.Source != entitlements.SourceApple && raw.Source != entitlements.SourceGoogle:
		return invalidf("unknown source %q", raw.Source)
	case strings.ContainsRune(raw.ID, 0) || len(raw.ID) > 256:
		return invalidf("bad id")
	}
	return nil
}
