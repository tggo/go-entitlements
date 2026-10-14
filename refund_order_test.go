package entitlements_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
)

var (
	e1 = t0.Add(30 * 24 * time.Hour) // transaction 1 expires
	e2 = t0.Add(60 * 24 * time.Hour) // transaction 2 expires
)

func txEvent(id string, state entitlements.State, signed time.Time, tx string, exp time.Time) entitlements.Event {
	return entitlements.Event{
		ID: id, Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "chain", ProductID: "pro",
		TransactionID: tx, State: state, OccurredAt: signed, ExpiresAt: exp,
	}
}

// fold applies events in the given order and returns the final entitlement;
// every event must be applied (nil error) unless wantStale is set for it.
func foldTx(t *testing.T, evs ...entitlements.Event) *entitlements.Entitlement {
	t.Helper()
	var cur *entitlements.Entitlement
	for _, e := range evs {
		next, err := entitlements.Apply(cur, e)
		require.NoError(t, err, e.ID)
		cur = next
	}
	return cur
}

// The REFUND of transaction 1 arrives before the renewal (transaction 2) that
// superseded it, and carries a later signedDate than the renewal.
func TestApply_RefundOfOldTransactionBeforeItsRenewal(t *testing.T) {
	got := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("refund-1", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx1", e1),
		txEvent("renew-2", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2), // signed before the refund, arrives after it
	)
	assert.Equal(t, entitlements.StateActive, got.State, "the renewal is a different, unrefunded transaction")
	assert.True(t, got.ExpiresAt.Equal(e2))
	assert.True(t, got.Active(e1.Add(time.Hour)))
	assert.Equal(t, []string{"tx1"}, got.RefundedTransactions)
	assert.True(t, got.UpdatedAt.Equal(t0.Add(2*time.Hour)), "UpdatedAt never moves backwards")
}

func TestApply_RenewalBeforeRefundOfOldTransaction(t *testing.T) {
	got := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("renew-2", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2),
		txEvent("refund-1", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx1", e1),
	)
	assert.Equal(t, entitlements.StateActive, got.State, "refunding the old transaction leaves the chain running")
	assert.True(t, got.ExpiresAt.Equal(e2))
	assert.Equal(t, []string{"tx1"}, got.RefundedTransactions)
	assert.Equal(t, int64(3), got.Version, "the refund is recorded, not dropped")
}

func TestApply_RefundOfLatestTransactionEndsTheChain(t *testing.T) {
	got := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("renew-2", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2),
		txEvent("refund-2", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx2", e2),
	)
	assert.Equal(t, entitlements.StateRefunded, got.State)
	assert.Equal(t, []string{"tx2"}, got.RefundedTransactions)

	// a late DID_RENEW for the refunded transaction must not undo it
	_, err := entitlements.Apply(got, txEvent("renew-2-again", entitlements.StateActive, t0.Add(3*time.Hour), "tx2", e2))
	require.ErrorIs(t, err, entitlements.ErrStale)
	assert.Contains(t, err.Error(), "refunded")
}

func TestApply_RefundReversalClearsTheRefundedTransaction(t *testing.T) {
	reversed := txEvent("reversed", entitlements.StateActive, t0.Add(3*time.Hour), "tx2", e2)
	reversed.Restores = true
	got := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("renew-2", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2),
		txEvent("refund-2", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx2", e2),
		reversed,
	)
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Empty(t, got.RefundedTransactions)
	// and now a renewal of that transaction is welcome again
	_, err := entitlements.Apply(got, txEvent("again", entitlements.StateActive, t0.Add(4*time.Hour), "tx2", e2))
	require.NoError(t, err)
}

func TestApply_RefundOfOldTransaction_StillNeedsASuccessorToRevive(t *testing.T) {
	cur := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("refund-1", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx1", e1),
	)
	// an older-signed event for the SAME (refunded) transaction stays stale
	_, err := entitlements.Apply(cur, txEvent("late", entitlements.StateActive, t0.Add(time.Hour), "tx1", e1))
	require.ErrorIs(t, err, entitlements.ErrStale)
	// an older-signed event for a transaction that expires no later does not revive either
	_, err = entitlements.Apply(cur, txEvent("same-expiry", entitlements.StateActive, t0.Add(time.Hour), "tx9", e1))
	require.ErrorIs(t, err, entitlements.ErrStale)
	// bookkeeping events (NoRevive) never lift, even for a newer transaction
	book := txEvent("status", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2)
	book.NoRevive = true
	_, err = entitlements.Apply(cur, book)
	require.ErrorIs(t, err, entitlements.ErrStale)
	// a non-active state of a newer transaction does not lift
	_, err = entitlements.Apply(cur, txEvent("grace", entitlements.StateGrace, t0.Add(time.Hour), "tx2", e2))
	require.ErrorIs(t, err, entitlements.ErrStale)
}

func TestApply_RefundBeforeTheTransactionItRefunds(t *testing.T) {
	// REFUND for tx2 arrives before tx2's own renewal event
	cur := foldTx(t,
		txEvent("sub", entitlements.StateActive, t0, "tx1", e1),
		txEvent("refund-2", entitlements.StateRefunded, t0.Add(2*time.Hour), "tx2", e2),
	)
	assert.Equal(t, entitlements.StateRefunded, cur.State)
	_, err := entitlements.Apply(cur, txEvent("renew-2", entitlements.StateActive, t0.Add(time.Hour), "tx2", e2))
	require.ErrorIs(t, err, entitlements.ErrStale)
}

func TestApply_RefundedTransactionsAreBounded(t *testing.T) {
	var cur *entitlements.Entitlement
	for i := range 100 {
		e := txEvent(fmt.Sprint("r", i), entitlements.StateRefunded, t0.Add(time.Duration(i)*time.Minute), fmt.Sprint("tx", i), e1)
		next, err := entitlements.Apply(cur, e)
		require.NoError(t, err)
		cur = next
	}
	require.Len(t, cur.RefundedTransactions, entitlements.MaxRefundedTransactions)
	assert.Equal(t, "tx99", cur.RefundedTransactions[len(cur.RefundedTransactions)-1], "the newest are kept")
	assert.Equal(t, fmt.Sprint("tx", 100-entitlements.MaxRefundedTransactions), cur.RefundedTransactions[0])
}

func TestApply_DoesNotMutateTheRefundedListOfItsInput(t *testing.T) {
	cur := &entitlements.Entitlement{
		OriginalTxID: "chain", Source: entitlements.SourceApple, State: entitlements.StateActive, ExpiresAt: e2, UpdatedAt: t0, Version: 1,
		RefundedTransactions: []string{"old"},
	}
	next, err := entitlements.Apply(cur, txEvent("r", entitlements.StateRefunded, t0.Add(time.Hour), "tx1", e1))
	require.NoError(t, err)
	assert.Equal(t, []string{"old"}, cur.RefundedTransactions)
	assert.Equal(t, []string{"old", "tx1"}, next.RefundedTransactions)
}
