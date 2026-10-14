package entitlements

import (
	"fmt"
	"time"
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
//     is how a resubscription on the same chain looks), or an event with
//     Restores set (REFUND_REVERSED) that is strictly newer. Later expired,
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
			Sandbox:      ev.Sandbox,
		}, nil
	}
	if ev.OccurredAt.Before(cur.UpdatedAt) {
		return nil, ErrStale
	}
	// A refund names one transaction. If the chain already extends beyond that
	// transaction's expiry, a newer transaction exists and the refund does not
	// end access: it is recorded but not applied. Equal expiry means the
	// refunded transaction is the latest. Apple only: other sources carry no
	// per-transaction expiry on refunds.
	if ev.Source == SourceApple && ev.State == StateRefunded && !isTerminal(cur.State) &&
		!ev.ExpiresAt.IsZero() && ev.ExpiresAt.Before(cur.ExpiresAt) {
		return nil, fmt.Errorf("%w: refund of an older transaction (expires %s) while chain runs to %s",
			ErrStale, ev.ExpiresAt.Format(time.RFC3339), cur.ExpiresAt.Format(time.RFC3339))
	}
	if isTerminal(cur.State) && !isTerminal(ev.State) {
		reactivates := (ev.State == StateActive || ev.Restores) && ev.OccurredAt.After(cur.UpdatedAt)
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
	next.Sandbox = ev.Sandbox
	next.UpdatedAt = ev.OccurredAt
	next.Version = cur.Version + 1
	return &next, nil
}
