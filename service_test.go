package entitlements_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/mocks"
	"github.com/tggo/go-entitlements/store/memory"
)

func newSvc(t *testing.T, st entitlements.Store) *entitlements.Service {
	return entitlements.NewService(st,
		entitlements.WithLogger(zaptest.NewLogger(t)),
		entitlements.WithTimeFn(func() time.Time { return t0.Add(time.Hour) }))
}

func TestService_Process_AppliesFn(t *testing.T) {
	st := mocks.NewMockStore(t)
	e := ev(entitlements.StateActive, t0)
	st.EXPECT().Apply(mock.Anything, e, mock.Anything).
		RunAndReturn(func(_ context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
			return fn(nil)
		})
	got, err := newSvc(t, st).Process(context.Background(), e)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Version)
}

func TestService_Process_Validation(t *testing.T) {
	st := mocks.NewMockStore(t)
	_, err := newSvc(t, st).Process(context.Background(), entitlements.Event{ID: "x"})
	require.Error(t, err)
}

func TestService_Process_PassesThroughSentinels(t *testing.T) {
	for _, sentinel := range []error{entitlements.ErrDuplicate, entitlements.ErrStale} {
		st := mocks.NewMockStore(t)
		st.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(nil, sentinel)
		_, err := newSvc(t, st).Process(context.Background(), ev(entitlements.StateActive, t0))
		assert.ErrorIs(t, err, sentinel)
	}
}

func TestService_Process_StoreError(t *testing.T) {
	boom := errors.New("boom")
	st := mocks.NewMockStore(t)
	st.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(nil, boom)
	_, err := newSvc(t, st).Process(context.Background(), ev(entitlements.StateActive, t0))
	assert.ErrorIs(t, err, boom)
}

func TestService_Supersede_StoreError(t *testing.T) {
	boom := errors.New("boom")
	st := mocks.NewMockStore(t)
	st.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything).Return(nil, boom).Once()
	e := ev(entitlements.StateActive, t0)
	e.SupersedesTxID = "old"
	_, err := newSvc(t, st).Process(context.Background(), e)
	assert.ErrorIs(t, err, boom)
}

func TestService_Supersede_Chain(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := newSvc(t, st)

	old := ev(entitlements.StateActive, t0)
	old.ID, old.OriginalTxID, old.Source = "e1", "tokA", entitlements.SourceGoogle
	_, err := svc.Process(ctx, old)
	require.NoError(t, err)

	nw := ev(entitlements.StateActive, t0.Add(time.Minute))
	nw.ID, nw.OriginalTxID, nw.Source, nw.SupersedesTxID = "e2", "tokB", entitlements.SourceGoogle, "tokA"
	_, err = svc.Process(ctx, nw)
	require.NoError(t, err)

	a, _ := st.Get(ctx, entitlements.SourceGoogle, "tokA")
	b, _ := st.Get(ctx, entitlements.SourceGoogle, "tokB")
	assert.Equal(t, entitlements.StateExpired, a.State)
	assert.Equal(t, entitlements.StateActive, b.State)

	// redelivery dedupes both the main and the synthetic event
	_, err = svc.Process(ctx, nw)
	assert.ErrorIs(t, err, entitlements.ErrDuplicate)

	// only the new chain grants access
	list, _ := st.ListByUser(ctx, "u1")
	assert.Len(t, list, 2)
	ok, err := svc.IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestService_Supersede_UnknownOldChainAndTerminalOld(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := newSvc(t, st)

	// old chain never seen
	nw := ev(entitlements.StateActive, t0)
	nw.ID, nw.OriginalTxID, nw.SupersedesTxID = "e2", "tokB", "ghost"
	_, err := svc.Process(ctx, nw)
	require.NoError(t, err)
	_, err = st.Get(ctx, entitlements.SourceApple, "ghost")
	assert.ErrorIs(t, err, entitlements.ErrNotFound)

	// old chain already refunded stays refunded
	r := ev(entitlements.StateRefunded, t0)
	r.ID, r.OriginalTxID = "e3", "tokR"
	_, err = svc.Process(ctx, r)
	require.NoError(t, err)
	n2 := ev(entitlements.StateActive, t0.Add(time.Minute))
	n2.ID, n2.OriginalTxID, n2.SupersedesTxID = "e4", "tokC", "tokR"
	_, err = svc.Process(ctx, n2)
	require.NoError(t, err)
	got, _ := st.Get(ctx, entitlements.SourceApple, "tokR")
	assert.Equal(t, entitlements.StateRefunded, got.State)
}

func TestService_IsActive(t *testing.T) {
	ctx := context.Background()
	st := mocks.NewMockStore(t)
	st.EXPECT().ListByUser(mock.Anything, "u1").Return([]entitlements.Entitlement{
		{ProductID: "pro", State: entitlements.StateExpired},
		{ProductID: "other", State: entitlements.StateActive, ExpiresAt: t0.Add(24 * time.Hour)},
	}, nil).Once()
	ok, err := newSvc(t, st).IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.False(t, ok)

	st.EXPECT().ListByUser(mock.Anything, "u2").Return(nil, errors.New("db down")).Once()
	_, err = newSvc(t, st).IsActive(ctx, "u2", "pro")
	assert.Error(t, err)
}

func TestService_IsActive_ExpiredByClock(t *testing.T) {
	st := mocks.NewMockStore(t)
	st.EXPECT().ListByUser(mock.Anything, "u1").Return([]entitlements.Entitlement{
		{ProductID: "pro", State: entitlements.StateActive, ExpiresAt: t0.Add(time.Minute)},
	}, nil)
	ok, err := newSvc(t, st).IsActive(context.Background(), "u1", "pro") // clock = t0+1h
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestNewService_DefaultsAreUsable(t *testing.T) {
	st := memory.New()
	svc := entitlements.NewService(st)
	_, err := svc.Process(context.Background(), ev(entitlements.StateActive, time.Now()))
	require.NoError(t, err)
	ok, err := svc.IsActive(context.Background(), "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok)
}
