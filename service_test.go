package entitlements_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/mocks"
	"github.com/tggo/go-entitlements/store/memory"
)

func newSvc(t *testing.T, st entitlements.Store) *entitlements.Service {
	t.Helper()
	return entitlements.NewService(st,
		entitlements.WithLogger(testutil.NewLogger(t)),
		entitlements.WithTimeFn(func() time.Time { return t0.Add(time.Hour) }))
}

func TestService_Process_AppliesFn(t *testing.T) {
	st := mocks.NewMockStore(t)
	e := ev(entitlements.StateActive, t0)
	st.EXPECT().Apply(mock.Anything, e, mock.Anything).
		RunAndReturn(func(_ context.Context, _ entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
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
	// ... gets an expired tombstone, so a late older event for it is stale
	// instead of resurrecting it (concurrency review #2).
	tomb, err := st.Get(ctx, entitlements.SourceApple, "ghost")
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, tomb.State)
	late := ev(entitlements.StateActive, t0.Add(-time.Hour))
	late.ID, late.OriginalTxID = "e-late", "ghost"
	_, err = svc.Process(ctx, late)
	assert.ErrorIs(t, err, entitlements.ErrStale)
	tomb, err = st.Get(ctx, entitlements.SourceApple, "ghost")
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, tomb.State)

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

func TestService_Process_InvalidEvent(t *testing.T) {
	good := ev(entitlements.StateActive, t0)
	tests := map[string]func(e *entitlements.Event){
		"no ID":          func(e *entitlements.Event) { e.ID = "" },
		"no source":      func(e *entitlements.Event) { e.Source = "" },
		"no original tx": func(e *entitlements.Event) { e.OriginalTxID = "" },
		"no state":       func(e *entitlements.Event) { e.State = "" },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			e := good
			mut(&e)
			_, err := newSvc(t, memory.New()).Process(context.Background(), e)
			require.ErrorIs(t, err, entitlements.ErrInvalidEvent)
		})
	}
}

func TestNewService_NilOptionsKeepDefaults(t *testing.T) {
	st := memory.New()
	svc := entitlements.NewService(st, nil, entitlements.WithLogger(nil), entitlements.WithTimeFn(nil))
	_, err := svc.Process(context.Background(), ev(entitlements.StateActive, time.Now()))
	require.NoError(t, err)
	ok, err := svc.IsActive(context.Background(), "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok)
}

// failingRetire fails the first Apply of the synthetic supersede event.
type failingRetire struct {
	entitlements.Store
	failures int
	order    []string
}

func (f *failingRetire) Apply(ctx context.Context, ev entitlements.Event, fn func(*entitlements.Entitlement) (*entitlements.Entitlement, error)) (*entitlements.Entitlement, error) {
	f.order = append(f.order, ev.ID)
	if strings.HasSuffix(ev.ID, "#supersede") && f.failures > 0 {
		f.failures--
		return nil, errors.New("transient")
	}
	return f.Store.Apply(ctx, ev, fn)
}

func TestService_Supersede_NewChainFirstAndRetireIsRetried(t *testing.T) {
	ctx := context.Background()
	st := &failingRetire{Store: memory.New(), failures: 1}
	svc := newSvc(t, st)

	old := ev(entitlements.StateActive, t0)
	old.ID, old.OriginalTxID = "e1", "tokA"
	_, err := svc.Process(ctx, old)
	require.NoError(t, err)

	nw := ev(entitlements.StateActive, t0.Add(time.Minute))
	nw.ID, nw.OriginalTxID, nw.SupersedesTxID = "e2", "tokB", "tokA"

	// The retire step fails: the new chain is already active, the old one too.
	_, err = svc.Process(ctx, nw)
	require.Error(t, err)
	assert.Equal(t, []string{"e1", "e2", "e2#supersede"}, st.order, "new chain is applied before the old one is retired")
	b, err := st.Get(ctx, entitlements.SourceApple, "tokB")
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, b.State)
	ok, err := svc.IsActive(ctx, "u1", "pro")
	require.NoError(t, err)
	assert.True(t, ok, "user keeps access through the failure")

	// Redelivery: main event is a duplicate, retire runs again and succeeds.
	_, err = svc.Process(ctx, nw)
	assert.ErrorIs(t, err, entitlements.ErrDuplicate)
	a, err := st.Get(ctx, entitlements.SourceApple, "tokA")
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateExpired, a.State)
	b, _ = st.Get(ctx, entitlements.SourceApple, "tokB")
	assert.Equal(t, int64(1), b.Version, "main event not applied twice")
}
