package apple_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// RealAppleDir holds real Apple-signed notifications (gitignored). Tests that
// use it skip when it is absent.
const realAppleDir = "../testdata/real/apple"

const realBundle = "menu.fridge.recipebox"

// realExpect keys on notificationUUID prefix-free: the file's notification type/subtype.
type realCase struct {
	err      error
	state    entitlements.State
	expires  int64 // ms
	occurred int64 // ms
}

func TestRealFixtures(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(realAppleDir, "*.json"))
	if len(files) == 0 {
		t.Skip("no real fixtures in testdata/real/apple")
	}
	want := map[string]realCase{
		"TEST":                   {err: entitlements.ErrIgnored},
		"ONE_TIME_CHARGE":        {err: entitlements.ErrIgnored},
		"SUBSCRIBED/INITIAL_BUY": {state: entitlements.StateActive, expires: 1790767344000, occurred: 1790680945335},
		"DID_CHANGE_RENEWAL_STATUS/AUTO_RENEW_DISABLED": {state: entitlements.StateActive, expires: 1790767344000, occurred: 1790681572251},
		"EXPIRED/VOLUNTARY":                             {state: entitlements.StateExpired, expires: 1790767344000, occurred: 1790767347942},
	}
	p := apple.NewParser(apple.NewVerifier(apple.WithBundleID(realBundle)))
	seen := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		require.NoError(t, err)
		n, err := apple.NewVerifier(apple.WithBundleID(realBundle)).VerifyNotification(body)
		require.NoError(t, err, filepath.Base(f))
		key := n.Payload.NotificationType
		if n.Payload.Subtype != "" {
			key += "/" + n.Payload.Subtype
		}
		w, ok := want[key]
		require.True(t, ok, "unexpected real notification %s", key)
		seen++
		ev, err := p.Parse(body)
		if w.err != nil {
			assert.ErrorIs(t, err, w.err, key)
			continue
		}
		require.NoError(t, err, key)
		assert.Equal(t, w.state, ev.State, key)
		assert.True(t, time.UnixMilli(w.expires).Equal(ev.ExpiresAt), key)
		assert.True(t, time.UnixMilli(w.occurred).Equal(ev.OccurredAt), key)
		assert.NotEmpty(t, ev.OriginalTxID, key)
		assert.Empty(t, ev.UserID, key) // fridge.menu sets no appAccountToken
	}
	assert.Equal(t, len(files), seen)
}
