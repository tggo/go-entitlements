package entitlements

import (
	"fmt"
	"slices"
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
//     error, so a late renewal-failure cannot undo a refund. An event with
//     NoRevive set never lifts a terminal state, active or not.
func Apply(cur *Entitlement, ev Event) (*Entitlement, error) {
	if cur == nil {
		e := &Entitlement{
			UserID:       ev.UserID,
			ProductID:    ev.ProductID,
			OriginalTxID: ev.OriginalTxID,
			Source:       ev.Source,
			State:        ev.State,
			ExpiresAt:    ev.ExpiresAt,
			UpdatedAt:    ev.OccurredAt,
			Version:      1,
			Environment:  ev.Environment,
			AutoRenew:    cloneBool(ev.AutoRenew),
		}
		if isTerminal(ev.State) {
			e.RefundedTransactions = withTx(nil, ev.TransactionID)
		}
		return e, nil
	}

	tx := ev.TransactionID
	refundedTx := tx != "" && slices.Contains(cur.RefundedTransactions, tx)
	// A renewal (a new, unrefunded transaction that outlasts the refunded one)
	// may arrive after the REFUND of the transaction it replaces, even if it
	// was signed earlier: the refund did not concern it.
	succeeds := isTerminal(cur.State) && ev.State == StateActive && !ev.NoRevive &&
		tx != "" && !refundedTx && ev.ExpiresAt.After(cur.ExpiresAt)

	if ev.OccurredAt.Before(cur.UpdatedAt) && !succeeds {
		return nil, ErrStale
	}
	if refundedTx && !isTerminal(ev.State) && !ev.Restores {
		return nil, fmt.Errorf("%w: transaction %s was refunded", ErrStale, tx)
	}

	next := *cur
	next.RefundedTransactions = slices.Clone(cur.RefundedTransactions)
	next.AutoRenew = cloneBool(cur.AutoRenew)

	// A refund names one transaction. If the chain already extends beyond that
	// transaction's expiry, a newer transaction exists and the refund does not
	// end access. With a TransactionID it is recorded (so that transaction's
	// own renewal cannot revive anything); without one it is dropped as stale.
	// Equal expiry means the refunded transaction is the latest. Apple only:
	// other sources carry no per-transaction expiry on refunds.
	if ev.Source == SourceApple && ev.State == StateRefunded && !isTerminal(cur.State) &&
		!ev.ExpiresAt.IsZero() && ev.ExpiresAt.Before(cur.ExpiresAt) {
		if tx == "" {
			return nil, fmt.Errorf("%w: refund of an older transaction (expires %s) while chain runs to %s",
				ErrStale, ev.ExpiresAt.Format(time.RFC3339), cur.ExpiresAt.Format(time.RFC3339))
		}
		next.RefundedTransactions = withTx(next.RefundedTransactions, tx)
		next.Version = cur.Version + 1
		next.UpdatedAt = laterOf(cur.UpdatedAt, ev.OccurredAt)
		return &next, nil
	}
	if isTerminal(cur.State) && !isTerminal(ev.State) {
		reactivates := !ev.NoRevive && (ev.State == StateActive || ev.Restores) && ev.OccurredAt.After(cur.UpdatedAt)
		if !reactivates && !succeeds {
			return nil, fmt.Errorf("%w: chain is %s, event wants %s", ErrStale, cur.State, ev.State)
		}
	}

	next.State = ev.State
	switch {
	case isTerminal(ev.State):
		next.RefundedTransactions = withTx(next.RefundedTransactions, tx)
	case ev.Restores && tx != "":
		next.RefundedTransactions = slices.DeleteFunc(next.RefundedTransactions, func(id string) bool { return id == tx })
	}
	if !ev.ExpiresAt.IsZero() {
		next.ExpiresAt = ev.ExpiresAt
	}
	if ev.ProductID != "" {
		next.ProductID = ev.ProductID
	}
	if ev.UserID != "" && cur.UserID == "" {
		next.UserID = ev.UserID // the first owner stays: see ErrUserConflict
	}
	if ev.AutoRenew != nil {
		next.AutoRenew = cloneBool(ev.AutoRenew)
	}
	next.Environment = ev.Environment
	next.UpdatedAt = laterOf(cur.UpdatedAt, ev.OccurredAt)
	next.Version = cur.Version + 1
	if len(next.RefundedTransactions) == 0 {
		next.RefundedTransactions = nil
	}
	return &next, nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// withTx appends id (if not empty or present) and keeps the newest
// MaxRefundedTransactions entries.
func withTx(list []string, id string) []string {
	if id == "" || slices.Contains(list, id) {
		return list
	}
	list = append(list, id)
	if len(list) > MaxRefundedTransactions {
		list = list[len(list)-MaxRefundedTransactions:]
	}
	return list
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// Clone returns a deep copy of e: the slice and pointer inside are copied, so
// the copy can be handed to callers or stored without aliasing.
func (e Entitlement) Clone() Entitlement {
	e.RefundedTransactions = slices.Clone(e.RefundedTransactions)
	e.AutoRenew = cloneBool(e.AutoRenew)
	return e
}
