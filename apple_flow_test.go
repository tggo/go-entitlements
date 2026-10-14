package entitlements_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
	"github.com/tggo/go-entitlements/internal/testutil"
	"github.com/tggo/go-entitlements/store/memory"
)

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := append(append(append([]int{}, p[:i]...), n-1), p[i:]...)
			out = append(out, q)
		}
	}
	return out
}

// runChain feeds three userless events (oldest first: subscribed, renewal
// status change, expired) through parser+Service+memory store in every
// delivery order and checks the end state is expired.
func runChain(t *testing.T, parser *apple.Parser, bodies [][]byte) {
	for _, order := range permutations(len(bodies)) {
		st := memory.New()
		svc := newSvc(t, st)
		var maxIdx = -1
		applied := 0
		for _, i := range order {
			ev, err := parser.Parse(bodies[i])
			require.NoError(t, err)
			assert.Empty(t, ev.UserID)
			_, err = svc.Process(context.Background(), ev)
			if i > maxIdx {
				maxIdx = i
				applied++
				require.NoError(t, err, "order %v", order)
			} else {
				assert.ErrorIs(t, err, entitlements.ErrStale, "order %v", order)
			}
		}
		ev, _ := parser.Parse(bodies[0])
		got, err := st.Get(context.Background(), entitlements.SourceApple, ev.OriginalTxID)
		require.NoError(t, err)
		assert.Equal(t, entitlements.StateExpired, got.State, "order %v", order)
		assert.Equal(t, int64(applied), got.Version, "order %v", order)
		assert.Empty(t, got.UserID)
	}
	// Delivered order: all three applied.
	st := memory.New()
	svc := newSvc(t, st)
	for _, b := range bodies {
		ev, err := parser.Parse(b)
		require.NoError(t, err)
		_, err = svc.Process(context.Background(), ev)
		require.NoError(t, err)
	}
	ev, _ := parser.Parse(bodies[0])
	got, err := st.Get(context.Background(), entitlements.SourceApple, ev.OriginalTxID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.Version)
}

func TestAppleChain_Synthetic_UserlessAllOrders(t *testing.T) {
	ca := testutil.NewAppleCA(t)
	var bodies [][]byte
	for i, name := range []string{"subscribed", "renewal_status_off", "expired"} {
		f := testutil.LoadAppleFixture(t, name)
		delete(f.Transaction, "appAccountToken")
		f.Transaction["expiresDate"] = float64(1760500000000)
		f.Notification["signedDate"] = float64(1760000000000 + int64(i)*1000)
		if name == "expired" {
			f.Notification["signedDate"] = float64(1760500001000)
		}
		bodies = append(bodies, f.SignBody(t, ca))
	}
	runChain(t, apple.NewParser(apple.NewVerifier(apple.WithRootPool(ca.Pool))), bodies)
}

func TestAppleChain_Real_UserlessAllOrders(t *testing.T) {
	dir := "testdata/real/apple"
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Skip("no real fixtures")
	}
	parser := apple.NewParser(apple.NewVerifier(apple.WithBundleID("menu.fridge.recipebox")))
	byType := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		n, err := apple.NewVerifier(apple.WithBundleID("menu.fridge.recipebox")).VerifyNotification(b)
		require.NoError(t, err)
		byType[n.Payload.NotificationType] = b
	}
	runChain(t, parser, [][]byte{byType["SUBSCRIBED"], byType["DID_CHANGE_RENEWAL_STATUS"], byType["EXPIRED"]})
}
