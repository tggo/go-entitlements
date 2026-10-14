package entitlements

import "context"

// Store persists entitlements and processed event IDs.
//
// Implementations are safe for concurrent use. Event IDs are deduplicated per
// Source. fn must be fast and pure: no I/O and no calls back into the Store
// (the memory store runs it under its mutex, Postgres inside a transaction
// that holds locks), and it may be called more than once when the store
// retries a transaction.
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
