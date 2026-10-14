// Package memory is an in-process entitlements.Store for tests and demos.
package memory

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/tggo/go-entitlements"
)

type key struct {
	source entitlements.Source
	txID   string
}

// Store keeps everything in maps behind one mutex.
//
// Apply semantics (shared with the Postgres store): a duplicate event ID
// returns ErrDuplicate without calling fn; if fn returns ErrStale, ErrIgnored
// or ErrUnsupported the event is recorded as processed (not applied) and the
// error is returned with the current entitlement; any other fn error leaves
// no trace so the event can be retried; fn returning (nil, nil) records the
// event and persists nothing.
type Store struct {
	mu        sync.Mutex
	items     map[key]entitlements.Entitlement
	processed map[string]struct{}
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		items:     map[key]entitlements.Entitlement{},
		processed: map[string]struct{}{},
	}
}

var _ entitlements.Store = (*Store)(nil)

func recordable(err error) bool {
	return errors.Is(err, entitlements.ErrStale) ||
		errors.Is(err, entitlements.ErrIgnored) ||
		errors.Is(err, entitlements.ErrUnsupported)
}

// Apply implements entitlements.Store.
func (s *Store) Apply(ctx context.Context, ev entitlements.Event, fn func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.processed[ev.ID]; dup {
		return nil, entitlements.ErrDuplicate
	}
	k := key{ev.Source, ev.OriginalTxID}
	var cur *entitlements.Entitlement
	if e, ok := s.items[k]; ok {
		c := e
		cur = &c
	}
	next, err := fn(cur)
	if err != nil {
		if recordable(err) {
			s.processed[ev.ID] = struct{}{}
		}
		return cur, err
	}
	s.processed[ev.ID] = struct{}{}
	if next == nil {
		return cur, nil
	}
	s.items[k] = *next
	out := *next
	return &out, nil
}

// Get implements entitlements.Store.
func (s *Store) Get(ctx context.Context, source entitlements.Source, originalTxID string) (*entitlements.Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[key{source, originalTxID}]
	if !ok {
		return nil, entitlements.ErrNotFound
	}
	return &e, nil
}

// ListByUser implements entitlements.Store. Results are ordered by
// (Source, OriginalTxID) for determinism.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]entitlements.Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []entitlements.Entitlement
	for _, e := range s.items {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].OriginalTxID < out[j].OriginalTxID
	})
	return out, nil
}
