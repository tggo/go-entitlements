package entitlements

import "context"

// Store persists entitlements and processed event IDs.
//
// Apply must be atomic per OriginalTxID: implementations lock the chain,
// call fn with the current entitlement (nil if none), and persist the result
// together with the event ID. A duplicate event ID returns ErrDuplicate
// without calling fn.
type Store interface {
	Apply(ctx context.Context, ev Event, fn func(cur *Entitlement) (*Entitlement, error)) (*Entitlement, error)
	Get(ctx context.Context, source Source, originalTxID string) (*Entitlement, error)
	ListByUser(ctx context.Context, userID string) ([]Entitlement, error)
}
