package entitlements

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Service runs events through the state machine against a Store.
type Service struct {
	store  Store
	timeFn func() time.Time
	log    *zap.Logger
}

// Option configures a Service.
type Option func(*Service)

// WithTimeFn injects the clock used by IsActive.
func WithTimeFn(fn func() time.Time) Option { return func(s *Service) { s.timeFn = fn } }

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) Option { return func(s *Service) { s.log = l } }

// NewService returns a Service backed by store.
func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store, timeFn: time.Now, log: zap.NewNop()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// supersedeSuffix derives the idempotency key of the synthetic event that
// retires the superseded chain.
const supersedeSuffix = "#supersede"

// Process applies one event. ErrDuplicate and ErrStale are returned as-is so
// the transport can answer 200 and stop store retries; the returned
// entitlement is the current state when the store provides one.
func (s *Service) Process(ctx context.Context, ev Event) (*Entitlement, error) {
	if ev.ID == "" || ev.Source == "" || ev.OriginalTxID == "" || ev.State == "" {
		return nil, fmt.Errorf("entitlements: event needs ID, Source, OriginalTxID and State")
	}

	// Retire the old chain first: if the main event then fails and is retried,
	// the synthetic event dedupes and nothing is applied twice.
	if ev.SupersedesTxID != "" && ev.SupersedesTxID != ev.OriginalTxID {
		if err := s.supersede(ctx, ev); err != nil {
			return nil, err
		}
	}

	ent, err := s.store.Apply(ctx, ev, func(cur *Entitlement) (*Entitlement, error) {
		return Apply(cur, ev)
	})
	switch {
	case err == nil:
		s.log.Info("event applied", zap.String("event", ev.ID), zap.String("state", string(ent.State)), zap.Int64("version", ent.Version))
	case errors.Is(err, ErrDuplicate), errors.Is(err, ErrStale):
		s.log.Debug("event not applied", zap.String("event", ev.ID), zap.Error(err))
	default:
		s.log.Error("apply failed", zap.String("event", ev.ID), zap.Error(err))
	}
	return ent, err
}

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
			return nil, nil // old chain never seen: nothing to retire
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
		return false, err
	}
	now := s.timeFn()
	for _, e := range list {
		if e.ProductID == productID && e.Active(now) {
			return true, nil
		}
	}
	return false, nil
}
