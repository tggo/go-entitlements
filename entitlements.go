// Package entitlements turns App Store Server Notifications v2 and Google Play
// Real-Time Developer Notifications into one ordered, idempotent subscription
// state per user and product.
package entitlements

import (
	"errors"
	"time"
)

// State is the normalized subscription state.
type State string

const (
	StateActive       State = "active"
	StateGrace        State = "grace"
	StateBillingRetry State = "billing_retry"
	StatePaused       State = "paused"
	StateExpired      State = "expired"
	StateRefunded     State = "refunded"
	StateRevoked      State = "revoked"
)

// Source is the store an event came from.
type Source string

const (
	SourceApple  Source = "apple"
	SourceGoogle Source = "google"
)

// Event is one normalized store notification.
type Event struct {
	ID           string // notificationUUID / Pub/Sub messageId — idempotency key
	Source       Source
	UserID       string // appAccountToken / obfuscatedExternalAccountId
	OriginalTxID string // originalTransactionId / purchaseToken
	// SupersedesTxID is set when this purchase replaces an older one
	// (Google linkedPurchaseToken).
	SupersedesTxID string
	ProductID      string
	State          State
	ExpiresAt      time.Time
	OccurredAt     time.Time // signedDate / eventTimeMillis — ordering key
	// Restores marks an event that lifts a refunded/revoked chain (Apple
	// REFUND_REVERSED). Without it only an active event can leave a terminal
	// state; with it any non-terminal state can, if strictly newer.
	Restores bool
	// Sandbox marks a non-production purchase: Apple environment
	// Sandbox/Xcode/LocalTesting, or Google testPurchase. Consumers should
	// not grant paid access for it in production.
	Sandbox bool
	// Environment is the store environment the event came from ("Production",
	// "Sandbox", "Xcode", "LocalTesting" for Apple). Empty if unknown.
	// Verifiers can pin one; consumers sharing a store between environments
	// should check it before calling Service.Process.
	Environment string
	Raw         []byte
}

// Entitlement is the current state of one subscription chain.
type Entitlement struct {
	UserID       string
	ProductID    string
	OriginalTxID string
	Source       Source
	State        State
	ExpiresAt    time.Time
	UpdatedAt    time.Time // OccurredAt of the last applied event
	Version      int64
	Sandbox      bool // from the last applied event, see Event.Sandbox
}

// Active reports whether the entitlement grants access at now.
func (e Entitlement) Active(now time.Time) bool {
	if e.State != StateActive && e.State != StateGrace {
		return false
	}
	return e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt)
}

var (
	// ErrDuplicate means the event ID was already processed.
	ErrDuplicate = errors.New("entitlements: duplicate event")
	// ErrStale means a newer event was already applied.
	ErrStale = errors.New("entitlements: stale event")
	// ErrUnsupported means the notification type is not mapped.
	ErrUnsupported = errors.New("entitlements: unsupported notification")
	// ErrIgnored means the notification carries no state (e.g. TEST).
	ErrIgnored = errors.New("entitlements: ignored notification")
	// ErrNotFound means no entitlement exists for the key.
	ErrNotFound = errors.New("entitlements: not found")
	// ErrInvalidSignature means the payload failed verification.
	ErrInvalidSignature = errors.New("entitlements: invalid signature")
)
