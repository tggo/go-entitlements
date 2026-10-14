package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// Service runs events through the state machine against a Store. It is safe
// for concurrent use.
type Service struct {
	store        Store
	timeFn       func() time.Time
	log          *slog.Logger
	allowSandbox bool
	resolver     UserResolver
	onChange     func(ctx context.Context, before, after *Entitlement)
	linkSeq      atomic.Int64
}

// UserResolver finds the user of an event that carries none, for example
// from your own originalTransactionId -> user table.
type UserResolver func(ctx context.Context, ev Event) (userID string, err error)

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

// WithUserResolver sets a lookup used by Process for events without a user
// ID, after the user of a superseded chain was tried. A returned user is
// applied to the event; an empty result applies it unattributed (attach a
// user later with Service.LinkUser); an error fails Process so the store
// redelivers the event. A nil resolver is ignored.
func WithUserResolver(r UserResolver) Option {
	return func(s *Service) {
		if r != nil {
			s.resolver = r
		}
	}
}

// WithOnChange registers a hook called after an entitlement change has been
// committed: a new chain, an applied event, a retired superseded chain or a
// user link. before is nil for a new chain. It is not called for duplicates,
// stale or ignored events. The hook runs synchronously on the calling
// goroutine; keep it fast or hand off. A panic is recovered and logged and
// never fails the event. A nil hook is ignored.
func WithOnChange(fn func(ctx context.Context, before, after *Entitlement)) Option {
	return func(s *Service) {
		if fn != nil {
			s.onChange = fn
		}
	}
}

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

	if ev.UserID == "" && s.resolver != nil {
		uid, err := s.resolver(ctx, ev)
		if err != nil {
			return nil, fmt.Errorf("entitlements: resolve user of event %s: %w", ev.ID, err)
		}
		if err := validate(Event{ID: ev.ID, Source: ev.Source, OriginalTxID: ev.OriginalTxID, State: ev.State, UserID: uid}); err != nil {
			return nil, fmt.Errorf("entitlements: resolver returned a bad user id: %w", err)
		}
		ev.UserID = uid
	}

	if ev.UserID == "" {
		s.log.WarnContext(ctx, "event has no user id; entitlement is keyed by transaction only", "event", ev.ID, "source", ev.Source, "tx", ev.OriginalTxID)
	}

	var before *Entitlement
	ent, err := s.store.Apply(ctx, ev, func(cur *Entitlement) (*Entitlement, error) {
		before = cloneEnt(cur) // the last call wins if the store retries
		return Apply(cur, ev)
	})
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "event applied", "event", ev.ID, "state", ent.State, "version", ent.Version)
		if ev.UserID != "" && ent.UserID != ev.UserID {
			s.log.WarnContext(ctx, "event names a different user than the chain owner; owner kept", "event", ev.ID, "owner", ent.UserID, "event_user", ev.UserID, "tx", ev.OriginalTxID)
		}
		s.notify(ctx, before, ent)
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
	var before *Entitlement
	after, err := s.store.Apply(ctx, old, func(cur *Entitlement) (*Entitlement, error) {
		before = cloneEnt(cur)
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
	if err == nil {
		s.notify(ctx, before, after)
		return nil
	}
	if errors.Is(err, ErrDuplicate) || errors.Is(err, ErrStale) {
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

func cloneEnt(e *Entitlement) *Entitlement {
	if e == nil {
		return nil
	}
	c := e.Clone()
	return &c
}

// notify calls the OnChange hook, if any, and keeps a panic in it from
// reaching the caller.
func (s *Service) notify(ctx context.Context, before, after *Entitlement) {
	if s.onChange == nil || after == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "OnChange hook panicked", "panic", fmt.Sprint(r), "tx", after.OriginalTxID)
		}
	}()
	s.onChange(ctx, before, after)
}

// LinkUser attaches userID to an existing chain that has no user, for the
// common case where the app learns the user after the purchase (it calls this
// with the originalTransactionId or purchase token). It is atomic and
// idempotent: linking the same user again changes nothing. Linking a chain
// that already belongs to a different user returns ErrUserConflict and never
// reassigns it. An unknown chain returns ErrNotFound.
func (s *Service) LinkUser(ctx context.Context, source Source, originalTxID, userID string) (*Entitlement, error) {
	if err := validate(Event{ID: "link", Source: source, OriginalTxID: originalTxID, State: StateActive, UserID: userID}); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, fmt.Errorf("%w: missing UserID", ErrInvalidEvent)
	}
	now := s.timeFn()
	ev := Event{
		// unique per call: idempotency comes from what the callback does, and a
		// shared ID would make a second link after ForgetUser look like a duplicate
		ID:     fmt.Sprintf("link:%d:%d", now.UnixNano(), s.linkSeq.Add(1)),
		Source: source, OriginalTxID: originalTxID, UserID: userID, State: StateActive, OccurredAt: now,
	}
	var before *Entitlement
	changed := false
	ent, err := s.store.Apply(ctx, ev, func(cur *Entitlement) (*Entitlement, error) {
		before, changed = cloneEnt(cur), false
		switch {
		case cur == nil:
			return nil, ErrNotFound
		case cur.UserID == userID:
			return nil, nil
		case cur.UserID != "":
			return nil, ErrUserConflict
		}
		next := cur.Clone()
		next.UserID = userID
		next.Version = cur.Version + 1
		changed = true
		return &next, nil
	})
	if err != nil {
		return nil, fmt.Errorf("entitlements: link user: %w", err)
	}
	if changed {
		s.log.InfoContext(ctx, "user linked", "source", source, "tx", originalTxID, "user", userID)
		s.notify(ctx, before, ent)
	}
	return ent, nil
}

// ForgetUser deletes everything stored for a user, for erasure requests: the
// user's entitlements (with their refunded-transaction lists) and, in stores
// that keep them, the raw webhook payloads of those chains. It returns the
// number of entitlements removed. Processed event IDs stay, so a redelivered
// webhook is still a duplicate and cannot recreate the chain. An empty
// userID is an error. Data you hold elsewhere (your user table, logs) is not
// touched.
func (s *Service) ForgetUser(ctx context.Context, userID string) (int, error) {
	if userID == "" {
		return 0, fmt.Errorf("%w: missing UserID", ErrInvalidEvent)
	}
	n, err := s.store.DeleteUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("entitlements: forget user: %w", err)
	}
	s.log.InfoContext(ctx, "user forgotten", "entitlements", n)
	return n, nil
}
