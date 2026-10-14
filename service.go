package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Service runs events through the state machine against a Store. It is safe
// for concurrent use.
type Service struct {
	store        Store
	timeFn       func() time.Time
	log          *slog.Logger
	allowSandbox bool
}

// Option configures a Service. Nil options are ignored.
type Option func(*Service)

// WithTimeFn injects the clock used by IsActive. A nil fn keeps time.Now.
func WithTimeFn(fn func() time.Time) Option {
	return func(s *Service) {
		if fn != nil {
			s.timeFn = fn
		}
	}
}

// WithLogger sets the logger. A nil logger keeps the default, which discards.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.log = l
		}
	}
}

// WithAllowSandbox lets Process apply, and IsActive grant access for,
// non-production events (Apple Sandbox/TestFlight/Xcode, Google test
// purchases). Without it Process returns ErrIgnored for them and IsActive
// skips sandbox entitlements already in the store, because a free test
// purchase must not unlock production features. Enable it only in test or
// staging deployments.
func WithAllowSandbox() Option { return func(s *Service) { s.allowSandbox = true } }

// NewService returns a Service backed by store.
func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store, timeFn: time.Now, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// supersedeSuffix derives the idempotency key of the synthetic event that
// retires the superseded chain.
const supersedeSuffix = "#supersede"

// Process applies one event. ErrDuplicate and ErrStale are returned as-is so
// the transport can answer 200 and stop store retries; the returned
// entitlement is the current state when the store provides one. An event
// missing ID, Source, OriginalTxID or State returns ErrInvalidEvent.
//
// When ev.SupersedesTxID is set and ev.UserID is empty, the user of the
// superseded chain is inherited. The new chain is applied first and the old
// chain is retired afterwards, so the user never has a moment without an
// active chain. If retiring fails, Process returns that error; redelivering
// the same event returns ErrDuplicate for the main event but retries the
// retire step, which is idempotent.
func (s *Service) Process(ctx context.Context, ev Event) (*Entitlement, error) {
	if err := validate(ev); err != nil {
		return nil, err
	}
	if ev.Sandbox() && !s.allowSandbox {
		s.log.InfoContext(ctx, "sandbox event ignored", "event", ev.ID, "environment", ev.Environment)
		return nil, fmt.Errorf("%w: %s event, build the Service WithAllowSandbox to process it", ErrIgnored, ev.Environment)
	}

	if ev.UserID == "" && ev.SupersedesTxID != "" && ev.SupersedesTxID != ev.OriginalTxID {
		// An upgrade token may carry no account id; the user is the one of the
		// chain it replaces.
		old, err := s.store.Get(ctx, ev.Source, ev.SupersedesTxID)
		switch {
		case err == nil:
			ev.UserID = old.UserID
		case errors.Is(err, ErrNotFound):
		default:
			return nil, fmt.Errorf("entitlements: look up superseded chain: %w", err)
		}
	}

	if ev.UserID == "" {
		s.log.WarnContext(ctx, "event has no user id; entitlement is keyed by transaction only", "event", ev.ID, "source", ev.Source, "tx", ev.OriginalTxID)
	}

	ent, err := s.store.Apply(ctx, ev, func(cur *Entitlement) (*Entitlement, error) {
		return Apply(cur, ev)
	})
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "event applied", "event", ev.ID, "state", ent.State, "version", ent.Version)
	case errors.Is(err, ErrDuplicate), errors.Is(err, ErrStale):
		s.log.DebugContext(ctx, "event not applied", "event", ev.ID, "reason", err)
	default:
		s.log.ErrorContext(ctx, "apply failed", "event", ev.ID, "error", err)
		return ent, err
	}

	if ev.SupersedesTxID != "" && ev.SupersedesTxID != ev.OriginalTxID {
		if serr := s.supersede(ctx, ev); serr != nil {
			s.log.ErrorContext(ctx, "supersede failed", "event", ev.ID, "old", ev.SupersedesTxID, "error", serr)
			return ent, serr
		}
	}
	return ent, err
}

// maxIDLen bounds identifiers a store has to index.
const maxIDLen = 256

func validate(ev Event) error {
	var missing string
	switch {
	case len(ev.ID) > maxIDLen || len(ev.OriginalTxID) > maxIDLen || len(ev.UserID) > maxIDLen:
		return fmt.Errorf("%w: ID, OriginalTxID or UserID longer than %d bytes", ErrInvalidEvent, maxIDLen)
	case strings.ContainsRune(ev.ID+ev.OriginalTxID+ev.UserID+ev.ProductID, 0):
		return fmt.Errorf("%w: NUL byte in an identifier", ErrInvalidEvent)
	case ev.ID == "":
		missing = "ID"
	case ev.Source == "":
		missing = "Source"
	case ev.OriginalTxID == "":
		missing = "OriginalTxID"
	case ev.State == "":
		missing = "State"
	default:
		return nil
	}
	return fmt.Errorf("%w: missing %s", ErrInvalidEvent, missing)
}

// supersede retires the old chain. A chain never seen before gets an expired
// tombstone row at the event's time, so a later, older event for it is stale
// instead of resurrecting it.
func (s *Service) supersede(ctx context.Context, ev Event) error {
	old := Event{
		ID:           ev.ID + supersedeSuffix,
		Source:       ev.Source,
		OriginalTxID: ev.SupersedesTxID,
		State:        StateExpired,
		OccurredAt:   ev.OccurredAt,
		Raw:          ev.Raw,
	}
	_, err := s.store.Apply(ctx, old, func(cur *Entitlement) (*Entitlement, error) {
		if cur == nil {
			return &Entitlement{
				Source:       old.Source,
				OriginalTxID: old.OriginalTxID,
				State:        StateExpired,
				UpdatedAt:    old.OccurredAt,
				Version:      1,
			}, nil
		}
		if isTerminal(cur.State) || cur.State == StateExpired {
			return nil, ErrStale
		}
		return Apply(cur, old)
	})
	if err == nil || errors.Is(err, ErrDuplicate) || errors.Is(err, ErrStale) {
		return nil
	}
	return fmt.Errorf("entitlements: supersede %s: %w", ev.SupersedesTxID, err)
}

// IsActive reports whether the user currently has access to productID.
func (s *Service) IsActive(ctx context.Context, userID, productID string) (bool, error) {
	list, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("entitlements: list by user: %w", err)
	}
	now := s.timeFn()
	for _, e := range list {
		if e.Sandbox() && !s.allowSandbox {
			continue
		}
		if e.ProductID == productID && e.Active(now) {
			return true, nil
		}
	}
	return false, nil
}

// Get returns the entitlement of one chain, or ErrNotFound.
func (s *Service) Get(ctx context.Context, source Source, originalTxID string) (*Entitlement, error) {
	e, err := s.store.Get(ctx, source, originalTxID)
	if err != nil {
		return nil, fmt.Errorf("entitlements: get: %w", err)
	}
	return e, nil
}

// ListByUser returns all entitlements attributed to userID, ordered by
// (Source, OriginalTxID). An empty userID returns nothing. Use IsActive to
// decide access; this is for showing a user their plans.
func (s *Service) ListByUser(ctx context.Context, userID string) ([]Entitlement, error) {
	list, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("entitlements: list by user: %w", err)
	}
	return list, nil
}
