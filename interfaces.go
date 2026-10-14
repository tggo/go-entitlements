package entitlements

import "context"

// Store persists entitlements and processed event IDs.
//
// The contract, checked by package storetest:
//
//  1. Apply is atomic per (Source, OriginalTxID). Concurrent calls for one
//     chain behave as if run one after another; each sees the previous
//     result.
//  2. Apply calls fn with the current entitlement (nil if the chain is new)
//     and persists fn's result together with the event ID in one step.
//  3. Event IDs are deduplicated per Source. A repeated (Source, ID) returns
//     ErrDuplicate without calling fn and without changing anything.
//  4. If fn returns ErrStale, ErrIgnored or ErrUnsupported (possibly
//     wrapped), the event ID is recorded as processed, nothing else changes,
//     and Apply returns the current entitlement together with that error.
//  5. If fn returns any other error, nothing is recorded or persisted: the
//     same event can be retried. Apply returns that error.
//  6. If fn returns (nil, nil), the event ID is recorded, nothing is
//     persisted, and Apply returns the current entitlement (nil if none).
//  7. On success Apply returns the saved entitlement with Source and
//     OriginalTxID set. ExpiresAt zero stays zero; Environment and
//     RefundedTransactions and AutoRenew round-trip, and a caller can neither see nor
//     change the stored list through a returned value.
//  8. fn must be fast and pure: no I/O and no calls back into the Store. A
//     store may call it more than once (it may retry a transaction), so the
//     outcome must depend only on its last return value.
//  9. Get returns ErrNotFound for an unknown chain; chains with the same
//     OriginalTxID under different Sources are independent.
//  10. ListByUser returns the user's entitlements ordered by (Source,
//     OriginalTxID). An empty userID matches nothing, so unattributed
//     entitlements are never returned as one user's.
//  11. All methods honour ctx: a cancelled context returns an error, and a
//     cancelled Apply does not consume the event ID.
//  12. Implementations are safe for concurrent use.
//  13. DeleteUser(userID) removes that user's entitlements across sources and
//     returns their count; other users' entitlements are untouched, an empty
//     userID deletes nothing, and processed event IDs are kept (a redelivered
//     event is still ErrDuplicate).
//
// Call Service.Process rather than Apply directly; the Service adds
// validation and supersede handling on top.
type Store interface {
	Apply(ctx context.Context, ev Event, fn func(cur *Entitlement) (*Entitlement, error)) (*Entitlement, error)
	Get(ctx context.Context, source Source, originalTxID string) (*Entitlement, error)
	ListByUser(ctx context.Context, userID string) ([]Entitlement, error)
	// DeleteUser removes every entitlement of userID and returns how many. A
	// store that keeps raw event payloads clears them for those chains too, but
	// keeps the processed event IDs so a redelivery stays a duplicate.
	DeleteUser(ctx context.Context, userID string) (int, error)
}
