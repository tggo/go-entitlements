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
	// StateActive grants access until ExpiresAt.
	StateActive State = "active"
	// StateGrace grants access while the store retries a failed payment;
	// ExpiresAt is the end of the grace period.
	StateGrace State = "grace"
	// StateBillingRetry means payment failed and the store keeps retrying
	// without a grace period; no access.
	StateBillingRetry State = "billing_retry"
	// StatePaused means the user paused the subscription (Google); no access.
	StatePaused State = "paused"
	// StateExpired means the subscription ended; no access.
	StateExpired State = "expired"
	// StateRefunded means the purchase was refunded; terminal, no access.
	StateRefunded State = "refunded"
	// StateRevoked means access was revoked (e.g. Family Sharing removed);
	// terminal, no access.
	StateRevoked State = "revoked"
)

// Environment says where a purchase was made. The zero value means
// production (the store did not say otherwise).
type Environment string

const (
	// EnvironmentProduction is a real, paid purchase.
	EnvironmentProduction Environment = "Production"
	// EnvironmentSandbox is Apple Sandbox or TestFlight: free test purchases.
	EnvironmentSandbox Environment = "Sandbox"
	// EnvironmentXcode is a locally signed Xcode StoreKit test purchase.
	EnvironmentXcode Environment = "Xcode"
	// EnvironmentLocalTesting is a StoreKit local-testing purchase.
	EnvironmentLocalTesting Environment = "LocalTesting"
	// EnvironmentGoogleTest is a Google Play license-tester purchase
	// (subscriptionsv2 testPurchase).
	EnvironmentGoogleTest Environment = "GoogleTest"
)

// Sandbox reports whether the environment is anything but production.
// Service ignores such events unless built WithAllowSandbox.
func (e Environment) Sandbox() bool { return e != "" && e != EnvironmentProduction }

// Source is the store an event came from.
type Source string

const (
	// SourceApple is the App Store.
	SourceApple Source = "apple"
	// SourceGoogle is Google Play.
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
	// NoRevive marks an event that must never lift a refunded/revoked chain,
	// even when its state is active. Apple sends bookkeeping notifications
	// (AUTO_RENEW_DISABLED, plan changes, offers, extensions) after a refund;
	// only a real purchase or renewal may bring the chain back.
	NoRevive bool
	// Environment is where the purchase was made. Empty means production.
	Environment Environment
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
	Environment  Environment // from the last applied event
}

// Sandbox reports whether this is a non-production entitlement.
func (e Entitlement) Sandbox() bool { return e.Environment.Sandbox() }

// Sandbox reports whether the event is a non-production purchase.
func (e Event) Sandbox() bool { return e.Environment.Sandbox() }

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
	// ErrInvalidEvent means the event lacks ID, Source, OriginalTxID or State.
	// Retrying cannot help; transports should acknowledge or reject it, not
	// ask the store to redeliver.
	ErrInvalidEvent = errors.New("entitlements: invalid event")
	// ErrInvalidSignature means the payload failed verification.
	ErrInvalidSignature = errors.New("entitlements: invalid signature")
)
