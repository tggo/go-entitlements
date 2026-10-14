package entitlements_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/store/memory"
)

func userless(id string, state entitlements.State, at time.Time) entitlements.Event {
	e := ev(state, at)
	e.ID, e.UserID = id, ""
	return e
}

func TestService_UserResolver(t *testing.T) {
	ctx := context.Background()

	t.Run("called for an event without a user and its answer is applied", func(t *testing.T) {
		var seen []entitlements.Event
		svc := entitlements.NewService(memory.New(), entitlements.WithUserResolver(func(_ context.Context, e entitlements.Event) (string, error) {
			seen = append(seen, e)
			return "linked-user", nil
		}))
		got, err := svc.Process(ctx, userless("e1", entitlements.StateActive, t0))
		require.NoError(t, err)
		assert.Equal(t, "linked-user", got.UserID)
		require.Len(t, seen, 1)
		assert.Equal(t, "tx1", seen[0].OriginalTxID)
		assert.Equal(t, "e1", seen[0].ID)
		list, err := svc.ListByUser(ctx, "linked-user")
		require.NoError(t, err)
		assert.Len(t, list, 1)
	})

	t.Run("not called when the event has a user", func(t *testing.T) {
		svc := entitlements.NewService(memory.New(), entitlements.WithUserResolver(func(context.Context, entitlements.Event) (string, error) {
			t.Error("resolver must not run")
			return "", nil
		}))
		_, err := svc.Process(ctx, ev(entitlements.StateActive, t0))
		require.NoError(t, err)
	})

	t.Run("not called when a superseded chain gave the user", func(t *testing.T) {
		st := memory.New()
		first := entitlements.NewService(st)
		old := ev(entitlements.StateActive, t0)
		old.ID, old.OriginalTxID = "o", "tokA"
		_, err := first.Process(ctx, old)
		require.NoError(t, err)

		svc := entitlements.NewService(st, entitlements.WithUserResolver(func(context.Context, entitlements.Event) (string, error) {
			t.Error("resolver must not run")
			return "", nil
		}))
		nw := userless("n", entitlements.StateActive, t0.Add(time.Minute))
		nw.OriginalTxID, nw.SupersedesTxID = "tokB", "tokA"
		got, err := svc.Process(ctx, nw)
		require.NoError(t, err)
		assert.Equal(t, "u1", got.UserID)
	})

	t.Run("an error is retryable: nothing is stored and the same event works next time", func(t *testing.T) {
		boom := errors.New("link table down")
		fail := true
		st := memory.New()
		svc := entitlements.NewService(st, entitlements.WithUserResolver(func(context.Context, entitlements.Event) (string, error) {
			if fail {
				return "", boom
			}
			return "u9", nil
		}))
		e := userless("e1", entitlements.StateActive, t0)
		_, err := svc.Process(ctx, e)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, entitlements.ErrInvalidEvent)
		_, err = st.Get(ctx, e.Source, e.OriginalTxID)
		require.ErrorIs(t, err, entitlements.ErrNotFound)

		fail = false
		got, err := svc.Process(ctx, e)
		require.NoError(t, err)
		assert.Equal(t, "u9", got.UserID)
	})

	t.Run("an empty answer applies the event unattributed", func(t *testing.T) {
		svc := entitlements.NewService(memory.New(), entitlements.WithUserResolver(func(context.Context, entitlements.Event) (string, error) { return "", nil }))
		got, err := svc.Process(ctx, userless("e1", entitlements.StateActive, t0))
		require.NoError(t, err)
		assert.Empty(t, got.UserID)
	})

	t.Run("an unusable answer is rejected and nothing is stored", func(t *testing.T) {
		for name, bad := range map[string]string{"NUL byte": "a\x00b", "too long": strings.Repeat("u", 300)} {
			st := memory.New()
			svc := entitlements.NewService(st, entitlements.WithUserResolver(func(context.Context, entitlements.Event) (string, error) { return bad, nil }))
			e := userless("e1", entitlements.StateActive, t0)
			_, err := svc.Process(ctx, e)
			require.ErrorIs(t, err, entitlements.ErrInvalidEvent, name)
			_, err = st.Get(ctx, e.Source, e.OriginalTxID)
			require.ErrorIs(t, err, entitlements.ErrNotFound, name)
		}
	})

	t.Run("a nil resolver is ignored", func(t *testing.T) {
		svc := entitlements.NewService(memory.New(), entitlements.WithUserResolver(nil))
		_, err := svc.Process(ctx, userless("e1", entitlements.StateActive, t0))
		require.NoError(t, err)
	})
}

func TestService_LinkUser(t *testing.T) {
	ctx := context.Background()
	newUserless := func(t *testing.T) (*entitlements.Service, *memory.Store) {
		t.Helper()
		st := memory.New()
		svc := newSvc(t, st)
		_, err := svc.Process(ctx, userless("e1", entitlements.StateActive, t0))
		require.NoError(t, err)
		return svc, st
	}

	t.Run("attaches a user to a chain without one", func(t *testing.T) {
		svc, _ := newUserless(t)
		got, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
		assert.Equal(t, "u5", got.UserID)
		assert.Equal(t, int64(2), got.Version)
		list, err := svc.ListByUser(ctx, "u5")
		require.NoError(t, err)
		require.Len(t, list, 1)
		ok, err := svc.IsActive(ctx, "u5", "pro")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("is idempotent", func(t *testing.T) {
		svc, _ := newUserless(t)
		first, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
		again, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
		assert.Equal(t, first.Version, again.Version, "a repeated link changes nothing")
		assert.Equal(t, "u5", again.UserID)
	})

	t.Run("a different user is a conflict and nothing changes", func(t *testing.T) {
		svc, _ := newUserless(t)
		_, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
		_, err = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "intruder")
		require.ErrorIs(t, err, entitlements.ErrUserConflict)
		got, err := svc.Get(ctx, entitlements.SourceApple, "tx1")
		require.NoError(t, err)
		assert.Equal(t, "u5", got.UserID)
		assert.Equal(t, int64(2), got.Version)
		// and the conflict does not poison a later correct link
		_, err = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
	})

	t.Run("an unknown chain is not found", func(t *testing.T) {
		svc, _ := newUserless(t)
		_, err := svc.LinkUser(ctx, entitlements.SourceApple, "nope", "u5")
		require.ErrorIs(t, err, entitlements.ErrNotFound)
	})

	t.Run("arguments are validated", func(t *testing.T) {
		svc, _ := newUserless(t)
		for _, c := range []struct {
			src      entitlements.Source
			tx, user string
		}{
			{"", "tx1", "u"}, {entitlements.SourceApple, "", "u"}, {entitlements.SourceApple, "tx1", ""}, {entitlements.SourceApple, "tx1", "a\x00b"},
		} {
			_, err := svc.LinkUser(ctx, c.src, c.tx, c.user)
			require.ErrorIs(t, err, entitlements.ErrInvalidEvent, c)
		}
	})

	t.Run("later events without a user keep the linked one", func(t *testing.T) {
		svc, _ := newUserless(t)
		_, err := svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
		require.NoError(t, err)
		got, err := svc.Process(ctx, userless("e2", entitlements.StateGrace, t0.Add(time.Hour)))
		require.NoError(t, err)
		assert.Equal(t, "u5", got.UserID)
		assert.Equal(t, entitlements.StateGrace, got.State)
	})

	t.Run("concurrent links of two users: exactly one wins", func(t *testing.T) {
		svc, _ := newUserless(t)
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				user := "ua"
				if i%2 == 1 {
					user = "ub"
				}
				_, errs[i] = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", user)
			}()
		}
		wg.Wait()
		var conflicts int
		for _, err := range errs {
			if errors.Is(err, entitlements.ErrUserConflict) {
				conflicts++
			} else {
				require.NoError(t, err)
			}
		}
		assert.Equal(t, 4, conflicts)
		got, err := svc.Get(ctx, entitlements.SourceApple, "tx1")
		require.NoError(t, err)
		assert.Equal(t, int64(2), got.Version)
	})
}

// A later event that names a different user does not take the chain over.
func TestService_ChainOwnerIsNotReassignedByLaterEvents(t *testing.T) {
	ctx := context.Background()
	var buf strings.Builder
	svc := entitlements.NewService(memory.New(), entitlements.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	_, err := svc.Process(ctx, ev(entitlements.StateActive, t0))
	require.NoError(t, err)

	other := ev(entitlements.StateGrace, t0.Add(time.Hour))
	other.ID, other.UserID = "e2", "someone-else"
	got, err := svc.Process(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID, "the owner stays")
	assert.Equal(t, entitlements.StateGrace, got.State, "the state still follows the event")
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "different user")
	list, err := svc.ListByUser(ctx, "someone-else")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestApply_ChainOwnerRules(t *testing.T) {
	cur := chain("apple", entitlements.StateActive, t0, t0.Add(time.Hour)) // owned by u1
	e := entitlements.Event{ID: "x", Source: entitlements.SourceApple, OriginalTxID: "tx1", State: entitlements.StateGrace, OccurredAt: t0.Add(time.Minute), UserID: "u2"}
	got, err := entitlements.Apply(cur, e)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID)

	noOwner := chain("apple", entitlements.StateActive, t0, t0.Add(time.Hour))
	noOwner.UserID = ""
	got, err = entitlements.Apply(noOwner, e)
	require.NoError(t, err)
	assert.Equal(t, "u2", got.UserID, "an unowned chain takes the first user an event names")
}

type recorderHook struct {
	mu    sync.Mutex
	calls []struct{ before, after *entitlements.Entitlement }
}

func (r *recorderHook) hook(_ context.Context, before, after *entitlements.Entitlement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct{ before, after *entitlements.Entitlement }{before, after})
}

func TestService_OnChange(t *testing.T) {
	ctx := context.Background()
	rec := &recorderHook{}
	svc := newSvcWith(t, memory.New(), entitlements.WithOnChange(rec.hook))

	_, err := svc.Process(ctx, ev(entitlements.StateActive, t0))
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)
	assert.Nil(t, rec.calls[0].before, "a new chain has no before")
	assert.Equal(t, entitlements.StateActive, rec.calls[0].after.State)

	grace := ev(entitlements.StateGrace, t0.Add(time.Hour))
	grace.ID = "e2"
	_, err = svc.Process(ctx, grace)
	require.NoError(t, err)
	require.Len(t, rec.calls, 2)
	assert.Equal(t, entitlements.StateActive, rec.calls[1].before.State)
	assert.Equal(t, entitlements.StateGrace, rec.calls[1].after.State)

	// duplicates and stale events do not call it
	_, err = svc.Process(ctx, grace)
	require.ErrorIs(t, err, entitlements.ErrDuplicate)
	old := ev(entitlements.StateExpired, t0)
	old.ID = "e0"
	_, err = svc.Process(ctx, old)
	require.ErrorIs(t, err, entitlements.ErrStale)
	assert.Len(t, rec.calls, 2)

	// a link and a supersede retirement are changes too
	_, err = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u1")
	require.NoError(t, err)
	assert.Len(t, rec.calls, 2, "linking the user the chain already has changes nothing")

	up := ev(entitlements.StateActive, t0.Add(2*time.Hour))
	up.ID, up.OriginalTxID, up.SupersedesTxID = "e3", "tokB", "tx1"
	_, err = svc.Process(ctx, up)
	require.NoError(t, err)
	require.Len(t, rec.calls, 4, "the new chain and the retired old one")
	assert.Equal(t, "tokB", rec.calls[2].after.OriginalTxID)
	assert.Equal(t, "tx1", rec.calls[3].after.OriginalTxID)
	assert.Equal(t, entitlements.StateExpired, rec.calls[3].after.State)
}

func TestService_OnChange_LinkUserIsAChange(t *testing.T) {
	ctx := context.Background()
	rec := &recorderHook{}
	svc := newSvcWith(t, memory.New(), entitlements.WithOnChange(rec.hook))
	_, err := svc.Process(ctx, userless("e1", entitlements.StateActive, t0))
	require.NoError(t, err)
	_, err = svc.LinkUser(ctx, entitlements.SourceApple, "tx1", "u5")
	require.NoError(t, err)
	require.Len(t, rec.calls, 2)
	assert.Empty(t, rec.calls[1].before.UserID)
	assert.Equal(t, "u5", rec.calls[1].after.UserID)
}

func TestService_OnChange_PanicIsRecovered(t *testing.T) {
	var buf strings.Builder
	svc := entitlements.NewService(memory.New(),
		entitlements.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
		entitlements.WithOnChange(func(context.Context, *entitlements.Entitlement, *entitlements.Entitlement) { panic("hook bug") }),
		entitlements.WithOnChange(nil))
	got, err := svc.Process(context.Background(), ev(entitlements.StateActive, t0))
	require.NoError(t, err, "a broken hook must not fail the event")
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Contains(t, buf.String(), "hook bug")
	assert.Contains(t, buf.String(), "level=ERROR")
}

func TestService_ForgetUser(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := newSvc(t, st)
	mine := ev(entitlements.StateActive, t0)
	other := ev(entitlements.StateActive, t0)
	other.ID, other.OriginalTxID, other.UserID = "o", "tx-other", "u2"
	for _, e := range []entitlements.Event{mine, other} {
		_, err := svc.Process(ctx, e)
		require.NoError(t, err)
	}

	n, err := svc.ForgetUser(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = svc.Get(ctx, entitlements.SourceApple, "tx1")
	require.ErrorIs(t, err, entitlements.ErrNotFound)
	list, err := svc.ListByUser(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = svc.Get(ctx, entitlements.SourceApple, "tx-other")
	require.NoError(t, err, "other users are untouched")

	// a redelivered event is still a duplicate: forgetting must not let it recreate the chain
	_, err = svc.Process(ctx, mine)
	require.ErrorIs(t, err, entitlements.ErrDuplicate)

	n, err = svc.ForgetUser(ctx, "u1")
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = svc.ForgetUser(ctx, "")
	require.ErrorIs(t, err, entitlements.ErrInvalidEvent)
}
