package entitlements_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	mocks "github.com/tggo/go-entitlements/internal/mocks/entitlements"
	"github.com/tggo/go-entitlements/store/memory"
)

func chain(src entitlements.Source, state entitlements.State, at time.Time, exp time.Time) *entitlements.Entitlement {
	return &entitlements.Entitlement{
		UserID: "u1", ProductID: "pro", OriginalTxID: "tx1", Source: src, State: state,
		ExpiresAt: exp, UpdatedAt: at, Version: 3,
	}
}

func refund(src entitlements.Source, at, exp time.Time) entitlements.Event {
	return entitlements.Event{ID: "r", Source: src, OriginalTxID: "tx1", State: entitlements.StateRefunded, OccurredAt: at, ExpiresAt: exp}
}

func TestApply_RefundOfOlderTransaction_Rules(t *testing.T) {
	later, earlier := t0.Add(60*24*time.Hour), t0.Add(30*24*time.Hour)

	t.Run("apple: older transaction does not end the chain", func(t *testing.T) {
		_, err := entitlements.Apply(chain(entitlements.SourceApple, entitlements.StateActive, t0, later), refund(entitlements.SourceApple, t0.Add(time.Hour), earlier))
		require.ErrorIs(t, err, entitlements.ErrStale)
		assert.Contains(t, err.Error(), "older transaction")
	})
	t.Run("apple: the latest transaction (equal expiry) ends it", func(t *testing.T) {
		got, err := entitlements.Apply(chain(entitlements.SourceApple, entitlements.StateActive, t0, later), refund(entitlements.SourceApple, t0.Add(time.Hour), later))
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateRefunded, got.State)
	})
	t.Run("google: the rule does not apply, an earlier ExpiresAt still refunds", func(t *testing.T) {
		got, err := entitlements.Apply(chain(entitlements.SourceGoogle, entitlements.StateActive, t0, later), refund(entitlements.SourceGoogle, t0.Add(time.Hour), earlier))
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateRefunded, got.State)
	})
	t.Run("apple: a refund without expiry refunds the chain", func(t *testing.T) {
		got, err := entitlements.Apply(chain(entitlements.SourceApple, entitlements.StateActive, t0, later), refund(entitlements.SourceApple, t0.Add(time.Hour), time.Time{}))
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateRefunded, got.State)
		assert.True(t, got.ExpiresAt.Equal(later), "a zero ExpiresAt does not overwrite the stored one")
	})
	for _, terminal := range []entitlements.State{entitlements.StateRefunded, entitlements.StateRevoked} {
		t.Run("apple: older refund on a "+string(terminal)+" chain is a terminal-to-terminal change", func(t *testing.T) {
			got, err := entitlements.Apply(chain(entitlements.SourceApple, terminal, t0, later), refund(entitlements.SourceApple, t0.Add(time.Hour), earlier))
			require.NoError(t, err, "handled by the terminal rule, not reported as an older transaction")
			assert.Equal(t, entitlements.StateRefunded, got.State)
			assert.Equal(t, int64(4), got.Version)
		})
	}
}

func TestApply_TerminalStates(t *testing.T) {
	exp := t0.Add(30 * 24 * time.Hour)
	blocked := []entitlements.State{entitlements.StateExpired, entitlements.StateBillingRetry, entitlements.StateGrace, entitlements.StatePaused}
	for _, terminal := range []entitlements.State{entitlements.StateRefunded, entitlements.StateRevoked} {
		t.Run(string(terminal), func(t *testing.T) {
			cur := chain(entitlements.SourceApple, terminal, t0, exp)
			for _, st := range blocked {
				e := ev(st, t0.Add(time.Hour))
				_, err := entitlements.Apply(cur, e)
				require.ErrorIs(t, err, entitlements.ErrStale, st)
			}
			got, err := entitlements.Apply(cur, ev(entitlements.StateActive, t0.Add(time.Hour)))
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateActive, got.State)

			restore := ev(entitlements.StateExpired, t0.Add(time.Hour))
			restore.Restores = true
			got, err = entitlements.Apply(cur, restore)
			require.NoError(t, err)
			assert.Equal(t, entitlements.StateExpired, got.State)

			// an equal timestamp is not "strictly after"
			_, err = entitlements.Apply(cur, ev(entitlements.StateActive, t0))
			require.ErrorIs(t, err, entitlements.ErrStale)

			other := entitlements.StateRevoked
			if terminal == entitlements.StateRevoked {
				other = entitlements.StateRefunded
			}
			got, err = entitlements.Apply(cur, ev(other, t0.Add(time.Hour)))
			require.NoError(t, err)
			assert.Equal(t, other, got.State)
		})
	}
}

func TestApply_Create_AllFields(t *testing.T) {
	e := entitlements.Event{
		ID: "e", Source: entitlements.SourceGoogle, UserID: "u9", OriginalTxID: "tokX", ProductID: "p9",
		State: entitlements.StateGrace, ExpiresAt: t0.Add(time.Hour), OccurredAt: t0, Environment: entitlements.EnvironmentGoogleTest,
	}
	got, err := entitlements.Apply(nil, e)
	require.NoError(t, err)
	assert.Equal(t, entitlements.Entitlement{
		UserID: "u9", ProductID: "p9", OriginalTxID: "tokX", Source: entitlements.SourceGoogle, State: entitlements.StateGrace,
		ExpiresAt: t0.Add(time.Hour), UpdatedAt: t0, Version: 1, Environment: entitlements.EnvironmentGoogleTest,
	}, *got)
}

func TestApply_UpdateKeepsAndReplacesFields(t *testing.T) {
	cur := chain(entitlements.SourceApple, entitlements.StateActive, t0, t0.Add(time.Hour))
	e := entitlements.Event{ID: "e", Source: entitlements.SourceApple, OriginalTxID: "tx1", State: entitlements.StateGrace, OccurredAt: t0.Add(time.Minute)}
	got, err := entitlements.Apply(cur, e)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID, "empty UserID keeps the stored one")
	assert.Equal(t, "pro", got.ProductID, "empty ProductID keeps the stored one")
	assert.True(t, got.ExpiresAt.Equal(t0.Add(time.Hour)), "zero ExpiresAt keeps the stored one")
	assert.Equal(t, int64(4), got.Version)
	assert.Equal(t, t0.Add(time.Minute), got.UpdatedAt)

	e.UserID, e.ProductID, e.ExpiresAt, e.Environment = "u2", "pro2", t0.Add(2*time.Hour), entitlements.EnvironmentSandbox
	e.ID, e.OccurredAt = "e2", t0.Add(2*time.Minute)
	got, err = entitlements.Apply(got, e)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID, "the first owner stays, a different user in a later event does not take over")
	assert.Equal(t, "pro2", got.ProductID)
	assert.True(t, got.ExpiresAt.Equal(t0.Add(2*time.Hour)))
	assert.Equal(t, entitlements.EnvironmentSandbox, got.Environment)
}

func TestService_Supersede_Rules(t *testing.T) {
	ctx := context.Background()

	t.Run("an already expired old chain is left alone", func(t *testing.T) {
		st := memory.New()
		svc := newSvc(t, st)
		old := ev(entitlements.StateActive, t0)
		old.ID, old.OriginalTxID = "o1", "tokA"
		_, err := svc.Process(ctx, old)
		require.NoError(t, err)
		gone := ev(entitlements.StateExpired, t0.Add(time.Minute))
		gone.ID, gone.OriginalTxID = "o2", "tokA"
		_, err = svc.Process(ctx, gone)
		require.NoError(t, err)
		before, err := st.Get(ctx, entitlements.SourceApple, "tokA")
		require.NoError(t, err)

		for i, id := range []string{"n1", "n2"} { // superseding twice must not touch it either
			nw := ev(entitlements.StateActive, t0.Add(time.Duration(10+i)*time.Minute))
			nw.ID, nw.OriginalTxID, nw.SupersedesTxID = id, "tokB"+id, "tokA"
			_, err = svc.Process(ctx, nw)
			require.NoError(t, err)
		}
		after, err := st.Get(ctx, entitlements.SourceApple, "tokA")
		require.NoError(t, err)
		assert.Equal(t, *before, *after, "version and UpdatedAt do not move")
	})

	t.Run("an event cannot supersede its own chain", func(t *testing.T) {
		st := memory.New()
		svc := newSvc(t, st)
		e := ev(entitlements.StateActive, t0)
		e.SupersedesTxID = e.OriginalTxID
		got, err := svc.Process(ctx, e)
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateActive, got.State)
		cur, err := st.Get(ctx, entitlements.SourceApple, "tx1")
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateActive, cur.State)
		assert.Equal(t, int64(1), cur.Version)
	})

	t.Run("a stale main event still retires the old chain", func(t *testing.T) {
		st := memory.New()
		svc := newSvc(t, st)
		old := ev(entitlements.StateActive, t0)
		old.ID, old.OriginalTxID = "o1", "tokA"
		_, err := svc.Process(ctx, old)
		require.NoError(t, err)
		newest := ev(entitlements.StateActive, t0.Add(10*time.Minute))
		newest.ID, newest.OriginalTxID = "n1", "tokB"
		_, err = svc.Process(ctx, newest)
		require.NoError(t, err)

		late := ev(entitlements.StateActive, t0.Add(time.Minute))
		late.ID, late.OriginalTxID, late.SupersedesTxID = "n0", "tokB", "tokA"
		_, err = svc.Process(ctx, late)
		require.ErrorIs(t, err, entitlements.ErrStale)
		a, err := st.Get(ctx, entitlements.SourceApple, "tokA")
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateExpired, a.State)
	})
}

func TestService_Process_StoreErrorReturnsTheCurrentEntitlement(t *testing.T) {
	boom := errors.New("boom")
	st := mocks.NewMockStore(t)
	cur := &entitlements.Entitlement{OriginalTxID: "tx1", Version: 7}
	st.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(cur, boom)
	got, err := newSvc(t, st).Process(context.Background(), ev(entitlements.StateActive, t0))
	require.ErrorIs(t, err, boom)
	assert.Same(t, cur, got)
}

func TestService_IsActive_UsesTheInjectedClock(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	exp := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC) // long before the wall clock
	e := ev(entitlements.StateActive, exp.Add(-24*time.Hour))
	e.ExpiresAt = exp
	_, err := entitlements.NewService(st).Process(ctx, e)
	require.NoError(t, err)

	before := entitlements.NewService(st, entitlements.WithTimeFn(func() time.Time { return exp.Add(-time.Hour) }))
	ok, err := before.IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok, "the injected clock is before the expiry")

	after := entitlements.NewService(st, entitlements.WithTimeFn(func() time.Time { return exp.Add(time.Hour) }))
	ok, err = after.IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = before.IsActive(ctx, "u1", "other-product")
	require.NoError(t, err)
	assert.False(t, ok, "another product")
}

// A skipped sandbox entitlement must not hide the production one behind it.
func TestService_IsActive_SandboxRowDoesNotShadowProduction(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	now := time.Now()
	sb := ev(entitlements.StateActive, now)
	sb.ID, sb.OriginalTxID, sb.Environment = "sb", "a-sandbox", entitlements.EnvironmentSandbox // sorts first
	prod := ev(entitlements.StateActive, now)
	prod.ID, prod.OriginalTxID = "pr", "b-production"
	allow := entitlements.NewService(st, entitlements.WithAllowSandbox())
	for _, e := range []entitlements.Event{sb, prod} {
		_, err := allow.Process(ctx, e)
		require.NoError(t, err)
	}
	ok, err := entitlements.NewService(st).IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok)
	list, err := entitlements.NewService(st).ListByUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a-sandbox", list[0].OriginalTxID)
}

func TestApply_AutoRenew(t *testing.T) {
	yes, no := true, false
	cur := chain(entitlements.SourceApple, entitlements.StateActive, t0, t0.Add(time.Hour))
	e := entitlements.Event{ID: "e", Source: entitlements.SourceApple, OriginalTxID: "tx1", State: entitlements.StateActive, OccurredAt: t0.Add(time.Minute)}

	got, err := entitlements.Apply(cur, e)
	require.NoError(t, err)
	assert.Nil(t, got.AutoRenew, "unknown stays unknown")

	e.AutoRenew = &yes
	got, err = entitlements.Apply(cur, e)
	require.NoError(t, err)
	require.NotNil(t, got.AutoRenew)
	assert.True(t, *got.AutoRenew)
	yes = false // the stored value must not alias the event's pointer
	assert.True(t, *got.AutoRenew)

	e.AutoRenew, e.ID, e.OccurredAt = &no, "e2", t0.Add(2*time.Minute)
	got, err = entitlements.Apply(got, e)
	require.NoError(t, err)
	assert.False(t, *got.AutoRenew)

	e.AutoRenew, e.ID, e.OccurredAt = nil, "e3", t0.Add(3*time.Minute)
	got, err = entitlements.Apply(got, e)
	require.NoError(t, err)
	require.NotNil(t, got.AutoRenew, "an event that does not say keeps the last known value")
	assert.False(t, *got.AutoRenew)

	created, err := entitlements.Apply(nil, entitlements.Event{ID: "n", Source: entitlements.SourceApple, OriginalTxID: "t", State: entitlements.StateActive, AutoRenew: &no})
	require.NoError(t, err)
	assert.False(t, *created.AutoRenew)
}
