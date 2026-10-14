// Package memory is an in-process entitlements.Store for tests and demos.
package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/tggo/go-entitlements"
)

type key struct {
	source entitlements.Source
	txID   string
}

// Store keeps everything in maps behind one mutex. It is safe for concurrent
// use; fn runs under the mutex and must not call back into the Store.
//
// Apply semantics (shared with the Postgres store): a duplicate event ID
// (per source) returns ErrDuplicate without calling fn; if fn returns ErrStale, ErrIgnored
// or ErrUnsupported the event is recorded as processed (not applied) and the
// error is returned with the current entitlement; any other fn error leaves
// no trace so the event can be retried; fn returning (nil, nil) records the
// event and persists nothing.
type Store struct {
	mu        sync.Mutex
	items     map[key]entitlements.Entitlement
	processed map[key]struct{} // keyed by (source, event ID); grows unbounded, fine for tests and demos
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		items:     map[key]entitlements.Entitlement{},
		processed: map[key]struct{}{},
	}
}

var _ entitlements.Store = (*Store)(nil)

func clone(e entitlements.Entitlement) entitlements.Entitlement { return e.Clone() }

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

	pk := key{ev.Source, ev.ID}
	if _, dup := s.processed[pk]; dup {
		return nil, entitlements.ErrDuplicate
	}
	k := key{ev.Source, ev.OriginalTxID}
	var cur *entitlements.Entitlement
	if e, ok := s.items[k]; ok {
		c := clone(e)
		cur = &c
	}
	next, err := fn(cur)
	if err != nil {
		if recordable(err) {
			s.processed[pk] = struct{}{}
		}
		return cur, err
	}
	s.processed[pk] = struct{}{}
	if next == nil {
		return cur, nil
	}
	s.items[k] = clone(*next)
	out := clone(*next)
	return &out, nil
}

// Get implements entitlements.Store.
func (s *Store) Get(ctx context.Context, source entitlements.Source, originalTxID string) (*entitlements.Entitlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[key{source, originalTxID}]
	if !ok {
		return nil, entitlements.ErrNotFound
	}
	e = clone(e)
	return &e, nil
}

// ListByUser implements entitlements.Store. Results are ordered by
// (Source, OriginalTxID) for determinism.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]entitlements.Entitlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, nil // userless entitlements (no appAccountToken) are not one user's
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []entitlements.Entitlement
	for _, e := range s.items {
		if e.UserID == userID {
			out = append(out, clone(e))
		}
	}
	slices.SortFunc(out, func(a, b entitlements.Entitlement) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.OriginalTxID, b.OriginalTxID))
	})
	return out, nil
}

// DeleteUser implements entitlements.Store. The processed event IDs stay.
func (s *Store) DeleteUser(ctx context.Context, userID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if userID == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, e := range s.items {
		if e.UserID == userID {
			delete(s.items, k)
			n++
		}
	}
	return n, nil
}
