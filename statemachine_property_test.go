package entitlements_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
)

// propSeed returns the seed for property tests: PROP_SEED if set, else a
// fresh one. It is logged when the test fails, so a failure reproduces with
// PROP_SEED=<n>.
func propSeed(t *testing.T) uint64 {
	t.Helper()
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv("PROP_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		require.NoError(t, err)
		seed = n
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("reproduce with PROP_SEED=%d", seed)
		}
	})
	return seed
}

var (
	nonTerminal = []entitlements.State{entitlements.StateActive, entitlements.StateGrace, entitlements.StateBillingRetry, entitlements.StatePaused, entitlements.StateExpired}
	allStates   = append(slices.Clone(nonTerminal), entitlements.StateRefunded, entitlements.StateRevoked)
)

// randomEvents builds n events with distinct timestamps.
func randomEvents(rnd *rand.Rand, n int, states []entitlements.State) []entitlements.Event {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := rnd.Perm(n * 3)[:n] // distinct offsets
	evs := make([]entitlements.Event, n)
	for i := range evs {
		at := base.Add(time.Duration(ts[i]) * time.Minute)
		evs[i] = entitlements.Event{
			ID: fmt.Sprint("e", i), Source: entitlements.SourceApple, UserID: "u", OriginalTxID: "tx",
			ProductID: fmt.Sprint("p", rnd.IntN(3)), State: states[rnd.IntN(len(states))],
			OccurredAt: at, ExpiresAt: at.Add(time.Duration(1+rnd.IntN(100)) * time.Hour),
			Sandbox: rnd.IntN(2) == 0, Restores: rnd.IntN(4) == 0,
		}
	}
	return evs
}

// foldAll applies events in order and returns the final entitlement.
func foldAll(t *testing.T, evs []entitlements.Event) *entitlements.Entitlement {
	t.Helper()
	var cur *entitlements.Entitlement
	for _, e := range evs {
		next, err := entitlements.Apply(cur, e)
		if err != nil {
			require.ErrorIs(t, err, entitlements.ErrStale)
			continue
		}
		cur = next
	}
	return cur
}

// Without terminal states, and with distinct timestamps, the result is
// determined by the newest event alone, whatever order events arrive in.
func TestApply_Property_OrderIndependentWithoutTerminalStates(t *testing.T) {
	seed := propSeed(t)
	rnd := rand.New(rand.NewPCG(seed, seed))
	for range 300 {
		evs := randomEvents(rnd, 2+rnd.IntN(12), nonTerminal)
		sorted := slices.Clone(evs)
		slices.SortFunc(sorted, func(a, b entitlements.Event) int { return a.OccurredAt.Compare(b.OccurredAt) })
		shuffled := slices.Clone(evs)
		rnd.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		want, got := foldAll(t, sorted), foldAll(t, shuffled)
		newest := sorted[len(sorted)-1]
		assert.Equal(t, newest.State, got.State)
		assert.Equal(t, want.State, got.State)
		assert.Equal(t, want.ExpiresAt, got.ExpiresAt)
		assert.Equal(t, want.ProductID, got.ProductID)
		assert.Equal(t, want.UpdatedAt, got.UpdatedAt)
		assert.Equal(t, want.Sandbox, got.Sandbox)
		if t.Failed() {
			t.Logf("events: %+v", evs)
			return
		}
	}
}

// For any sequence, including terminal states and any order: the version
// grows by exactly one per applied event, UpdatedAt never goes back, the
// input is never mutated, and a refunded or revoked chain only leaves that
// state through a terminal event, an active event or a Restores event that is
// strictly newer.
func TestApply_Property_InvariantsAnyOrder(t *testing.T) {
	seed := propSeed(t)
	rnd := rand.New(rand.NewPCG(seed, seed+1))
	for range 300 {
		evs := randomEvents(rnd, 2+rnd.IntN(15), allStates)
		rnd.Shuffle(len(evs), func(i, j int) { evs[i], evs[j] = evs[j], evs[i] })
		var cur *entitlements.Entitlement
		for _, e := range evs {
			var before entitlements.Entitlement
			if cur != nil {
				before = *cur
			}
			next, err := entitlements.Apply(cur, e)
			if cur != nil {
				require.Equal(t, before, *cur, "Apply mutated its input")
			}
			if err != nil {
				require.ErrorIs(t, err, entitlements.ErrStale)
				require.Nil(t, next)
				continue
			}
			if cur == nil {
				require.Equal(t, int64(1), next.Version)
			} else {
				require.Equal(t, cur.Version+1, next.Version)
				require.False(t, next.UpdatedAt.Before(cur.UpdatedAt))
				if (cur.State == entitlements.StateRefunded || cur.State == entitlements.StateRevoked) &&
					next.State != entitlements.StateRefunded && next.State != entitlements.StateRevoked {
					require.True(t, e.State == entitlements.StateActive || e.Restores, "left a terminal state via %s", e.State)
					require.True(t, e.OccurredAt.After(cur.UpdatedAt))
				}
			}
			cur = next
		}
	}
}
