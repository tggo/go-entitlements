package entitlements

import (
	"fmt"
)

func isTerminal(s State) bool { return s == StateRefunded || s == StateRevoked }

// Apply is the pure state machine: it folds one event into the current
// entitlement and returns the next one. cur is never mutated.
//
// Rules:
//   - cur == nil creates a new entitlement at Version 1.
//   - An event older than cur.UpdatedAt returns ErrStale; an event with an
//     equal timestamp is applied (stores deliver in whole milliseconds/seconds,
//     so ties are common and dropping them would lose state).
//   - Refunded and revoked are terminal for the chain: a later event moves it
//     out only if it is itself terminal (refunded <-> revoked) or explicitly
//     re-activates it (State active, strictly after the terminal event, which
//     is how a resubscription on the same chain looks). Later expired,
//     billing_retry, grace or paused events are ignored with an ErrStale-wrapped
//     error, so a late renewal-failure cannot undo a refund.
func Apply(cur *Entitlement, ev Event) (*Entitlement, error) {
	if cur == nil {
		return &Entitlement{
			UserID:       ev.UserID,
			ProductID:    ev.ProductID,
			OriginalTxID: ev.OriginalTxID,
			Source:       ev.Source,
			State:        ev.State,
			ExpiresAt:    ev.ExpiresAt,
			UpdatedAt:    ev.OccurredAt,
			Version:      1,
		}, nil
	}
	if ev.OccurredAt.Before(cur.UpdatedAt) {
		return nil, ErrStale
	}
	if isTerminal(cur.State) && !isTerminal(ev.State) {
		reactivates := ev.State == StateActive && ev.OccurredAt.After(cur.UpdatedAt)
		if !reactivates {
			return nil, fmt.Errorf("%w: chain is %s, event wants %s", ErrStale, cur.State, ev.State)
		}
	}
	next := *cur
	next.State = ev.State
	if !ev.ExpiresAt.IsZero() {
		next.ExpiresAt = ev.ExpiresAt
	}
	if ev.ProductID != "" {
		next.ProductID = ev.ProductID
	}
	if ev.UserID != "" {
		next.UserID = ev.UserID
	}
	next.UpdatedAt = ev.OccurredAt
	next.Version = cur.Version + 1
	return &next, nil
}
