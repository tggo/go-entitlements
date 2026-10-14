package entitlements_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
)

// FuzzApply: the state machine must never panic, must leave its input
// untouched, and returns either a new entitlement or an error, never both.
func FuzzApply(f *testing.F) {
	f.Add("active", "grace", int64(100), int64(200), int64(1000), int64(2000), true, false, true)
	f.Add("refunded", "active", int64(500), int64(600), int64(0), int64(0), false, true, false)
	f.Add("", "garbage", int64(-1), int64(1<<62), int64(-1<<62), int64(0), true, true, true)
	f.Fuzz(func(t *testing.T, curState, evState string, curAt, evAt, curExp, evExp int64, hasCur, restores, sandbox bool) {
		var cur *entitlements.Entitlement
		if hasCur {
			cur = &entitlements.Entitlement{
				UserID: "u", ProductID: "p", OriginalTxID: "tx", Source: entitlements.SourceApple,
				State: entitlements.State(curState), UpdatedAt: time.UnixMilli(curAt).UTC(),
				ExpiresAt: time.UnixMilli(curExp).UTC(), Version: 3,
			}
		}
		var before entitlements.Entitlement
		if cur != nil {
			before = *cur
		}
		ev := entitlements.Event{
			ID: "e", Source: entitlements.SourceApple, OriginalTxID: "tx", State: entitlements.State(evState),
			OccurredAt: time.UnixMilli(evAt).UTC(), ExpiresAt: time.UnixMilli(evExp).UTC(),
			Restores: restores, Sandbox: sandbox,
		}
		next, err := entitlements.Apply(cur, ev)
		if cur != nil {
			require.Equal(t, before, *cur)
		}
		if err != nil {
			require.Nil(t, next)
			return
		}
		require.NotNil(t, next)
		_ = next.Active(time.UnixMilli(evAt))
	})
}
