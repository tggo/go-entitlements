package entitlements_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ev(state entitlements.State, at time.Time) entitlements.Event {
	return entitlements.Event{
		ID: "e", Source: entitlements.SourceApple, UserID: "u1", OriginalTxID: "tx1",
		ProductID: "pro", State: state, ExpiresAt: at.Add(30 * 24 * time.Hour), OccurredAt: at,
	}
}

func TestApply_Create(t *testing.T) {
	got, err := entitlements.Apply(nil, ev(entitlements.StateActive, t0))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, got.State)
	assert.Equal(t, int64(1), got.Version)
	assert.Equal(t, t0, got.UpdatedAt)
	assert.Equal(t, "u1", got.UserID)
}

func TestApply_Transitions(t *testing.T) {
	tests := []struct {
		name    string
		cur     entitlements.State
		ev      entitlements.State
		delta   time.Duration
		want    entitlements.State
		wantErr error
	}{
		{"renew", entitlements.StateActive, entitlements.StateActive, time.Hour, entitlements.StateActive, nil},
		{"grace to active", entitlements.StateGrace, entitlements.StateActive, time.Hour, entitlements.StateActive, nil},
		{"equal timestamp applied", entitlements.StateActive, entitlements.StateExpired, 0, entitlements.StateExpired, nil},
		{"stale", entitlements.StateActive, entitlements.StateExpired, -time.Second, "", entitlements.ErrStale},
		{"refund then late expire ignored", entitlements.StateRefunded, entitlements.StateExpired, time.Hour, "", entitlements.ErrStale},
		{"refund then later billing retry ignored", entitlements.StateRefunded, entitlements.StateBillingRetry, time.Hour, "", entitlements.ErrStale},
		{"refund then equal-time active ignored", entitlements.StateRefunded, entitlements.StateActive, 0, "", entitlements.ErrStale},
		{"refund then later active reactivates", entitlements.StateRefunded, entitlements.StateActive, time.Hour, entitlements.StateActive, nil},
		{"revoked then refunded", entitlements.StateRevoked, entitlements.StateRefunded, time.Hour, entitlements.StateRefunded, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := &entitlements.Entitlement{UserID: "u1", ProductID: "pro", OriginalTxID: "tx1", Source: entitlements.SourceApple, State: tt.cur, UpdatedAt: t0, Version: 3}
			snapshot := *cur
			got, err := entitlements.Apply(cur, ev(tt.ev, t0.Add(tt.delta)))
			assert.Equal(t, snapshot, *cur, "input must not be mutated")
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.State)
			assert.Equal(t, int64(4), got.Version)
			assert.Equal(t, t0.Add(tt.delta), got.UpdatedAt)
		})
	}
}

func TestApply_KeepsFieldsWhenEventOmitsThem(t *testing.T) {
	cur := &entitlements.Entitlement{UserID: "u1", ProductID: "pro", State: entitlements.StateActive, ExpiresAt: t0.Add(time.Hour), UpdatedAt: t0, Version: 1}
	e := entitlements.Event{State: entitlements.StateGrace, OccurredAt: t0.Add(time.Minute)}
	got, err := entitlements.Apply(cur, e)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID)
	assert.Equal(t, "pro", got.ProductID)
	assert.Equal(t, cur.ExpiresAt, got.ExpiresAt)
	assert.Equal(t, entitlements.StateGrace, got.State)
}
