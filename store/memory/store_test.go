package memory_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/memory"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func event(id string, state entitlements.State, at time.Time) entitlements.Event {
	return entitlements.Event{ID: id, Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "tx1",
		ProductID: "pro", State: state, ExpiresAt: at.Add(time.Hour), OccurredAt: at}
}

func process(st *memory.Store, e entitlements.Event) (*entitlements.Entitlement, error) {
	return st.Apply(context.Background(), e, func(cur *entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return entitlements.Apply(cur, e)
	})
}

func TestStore_Duplicate(t *testing.T) {
	st := memory.New()
	_, err := process(st, event("a", entitlements.StateActive, t0))
	require.NoError(t, err)
	called := false
	_, err = st.Apply(context.Background(), event("a", entitlements.StateActive, t0), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
		called = true
		return nil, nil
	})
	assert.ErrorIs(t, err, entitlements.ErrDuplicate)
	assert.False(t, called)
}

func TestStore_StaleIsRecorded(t *testing.T) {
	st := memory.New()
	_, err := process(st, event("a", entitlements.StateActive, t0))
	require.NoError(t, err)
	cur, err := process(st, event("b", entitlements.StateExpired, t0.Add(-time.Minute)))
	assert.ErrorIs(t, err, entitlements.ErrStale)
	require.NotNil(t, cur)
	assert.Equal(t, int64(1), cur.Version)
	// recorded: redelivery is a duplicate, not a second stale
	_, err = process(st, event("b", entitlements.StateExpired, t0.Add(-time.Minute)))
	assert.ErrorIs(t, err, entitlements.ErrDuplicate)
}

func TestStore_OtherErrorIsNotRecorded(t *testing.T) {
	st := memory.New()
	boom := errors.New("boom")
	_, err := st.Apply(context.Background(), event("a", entitlements.StateActive, t0), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return nil, boom
	})
	assert.ErrorIs(t, err, boom)
	_, err = process(st, event("a", entitlements.StateActive, t0))
	assert.NoError(t, err, "failed event must be retryable")
}

func TestStore_NilResultPersistsNothing(t *testing.T) {
	st := memory.New()
	cur, err := st.Apply(context.Background(), event("a", entitlements.StateActive, t0), func(*entitlements.Entitlement) (*entitlements.Entitlement, error) {
		return nil, nil
	})
	require.NoError(t, err)
	assert.Nil(t, cur)
	_, err = st.Get(context.Background(), entitlements.SourceApple, "tx1")
	assert.ErrorIs(t, err, entitlements.ErrNotFound)
}

func TestStore_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := memory.New().Apply(ctx, event("a", entitlements.StateActive, t0), nil)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestStore_ListByUser(t *testing.T) {
	st := memory.New()
	for i, tx := range []string{"b", "a"} {
		e := event(fmt.Sprint(i), entitlements.StateActive, t0)
		e.OriginalTxID = tx
		_, err := process(st, e)
		require.NoError(t, err)
	}
	other := event("x", entitlements.StateActive, t0)
	other.UserID, other.OriginalTxID = "u2", "z"
	_, _ = process(st, other)

	list, err := st.ListByUser(context.Background(), "u1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].OriginalTxID)
	assert.Equal(t, "b", list[1].OriginalTxID)
}

func TestStore_Concurrent(t *testing.T) {
	st := memory.New()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = process(st, event(fmt.Sprint("e", i), entitlements.StateActive, t0.Add(time.Duration(i)*time.Second)))
		}()
	}
	wg.Wait()
	got, err := st.Get(context.Background(), entitlements.SourceApple, "tx1")
	require.NoError(t, err)
	assert.Equal(t, t0.Add((n-1)*time.Second), got.UpdatedAt)
	assert.LessOrEqual(t, got.Version, int64(n))
}
