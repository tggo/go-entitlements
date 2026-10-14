package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tggo/go-entitlements"
)

var (
	// ErrMalformed means the push body or its payload could not be decoded.
	ErrMalformed = errors.New("google: malformed notification")
	// ErrPackageMismatch means the notification is for another application.
	ErrPackageMismatch = errors.New("google: package name mismatch")
	// ErrNoExpiry means an access-granting subscription (active, canceled or
	// in grace) came back with no line item expiryTime. Granting it would give
	// open-ended access, so the event is refused (it wraps
	// entitlements.ErrUnsupported). The next notification repairs the state.
	ErrNoExpiry = fmt.Errorf("google: granting subscription has no expiryTime: %w", entitlements.ErrUnsupported)
	// ErrTestPurchase is returned for license-tester purchases when
	// WithRejectTestPurchases is set (it wraps entitlements.ErrIgnored).
	ErrTestPurchase = fmt.Errorf("google: test purchase: %w", entitlements.ErrIgnored)

	// ErrPurchaseNotFound means subscriptionsv2.get answered 404 or 410. The token is
	// unknown or too old; redelivery cannot fix it, so acknowledge (HTTP 200).
	ErrPurchaseNotFound = errors.New("google: purchase token not found or expired")
	// ErrFetchConfig means the Play API answered 401 or 403. The service account or
	// its Play Console permissions are misconfigured; retry will not help until
	// an operator fixes it. Answer 500 and log loudly.
	ErrFetchConfig = errors.New("google: play api credentials or permissions rejected")
	// ErrFetchTransient means 5xx, 429, timeouts and transport failures. Answer 500
	// so Pub/Sub redelivers.
	ErrFetchTransient = errors.New("google: play api transient failure")
)

// DefaultGraceDuration is used to size the access window of a subscription in
// grace period when Google's expiryTime is not in the future. 7 days is the
// Play Console default for the grace period.
const DefaultGraceDuration = 7 * 24 * time.Hour

// DefaultFetchTimeout bounds one subscriptionsv2.get call.
const DefaultFetchTimeout = 15 * time.Second

// Parser turns RTDN Pub/Sub push bodies into entitlements.Event. It is safe
// for concurrent use.
type Parser struct {
	fetcher     SubscriptionsFetcher
	packageName string
	rejectTests bool
	grace       time.Duration
	fetchTO     time.Duration
}

// Option configures a Parser.
type Option func(*Parser)

// WithPackageName rejects notifications for any other packageName.
func WithPackageName(name string) Option {
	return func(p *Parser) { p.packageName = name }
}

// WithRejectTestPurchases makes the parser return ErrTestPurchase for
// license-tester purchases. Default: they are parsed like real ones and
// flagged with Event.Sandbox, so QA keeps working on production builds.
func WithRejectTestPurchases() Option {
	return func(p *Parser) { p.rejectTests = true }
}

// WithGraceDuration sets the length of the access window granted in the
// IN_GRACE_PERIOD state (default DefaultGraceDuration). Match it to the grace
// period configured for your base plans in the Play Console.
func WithGraceDuration(d time.Duration) Option {
	return func(p *Parser) { p.grace = d }
}

// WithFetchTimeout bounds each subscriptionsv2.get call (default
// DefaultFetchTimeout). Non-positive values keep the default.
func WithFetchTimeout(d time.Duration) Option {
	return func(p *Parser) {
		if d > 0 {
			p.fetchTO = d
		}
	}
}

// NewParser builds a Parser. fetcher is used for subscription notifications
// and must not be nil. Nil options are ignored.
func NewParser(fetcher SubscriptionsFetcher, opts ...Option) (*Parser, error) {
	if fetcher == nil {
		return nil, errors.New("google: NewParser: fetcher is nil")
	}
	p := &Parser{fetcher: fetcher, grace: DefaultGraceDuration, fetchTO: DefaultFetchTimeout}
	for _, o := range opts {
		if o != nil {
			o(p)
		}
	}
	return p, nil
}

// Parse decodes a Pub/Sub push body and maps it to an Event. It returns
// entitlements.ErrIgnored for test notifications (and pending purchases) and
// entitlements.ErrUnsupported for notifications or states it cannot map.
func (p *Parser) Parse(ctx context.Context, body []byte) (entitlements.Event, error) {
	var env PushEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return entitlements.Event{}, fmt.Errorf("%w: envelope: %w", ErrMalformed, err)
	}
	if env.Message.MessageID == "" || len(env.Message.Data) == 0 {
		return entitlements.Event{}, fmt.Errorf("%w: missing messageId or data", ErrMalformed)
	}
	var dn DeveloperNotification
	if err := json.Unmarshal(env.Message.Data, &dn); err != nil {
		return entitlements.Event{}, fmt.Errorf("%w: payload: %w", ErrMalformed, err)
	}
	if p.packageName != "" && dn.PackageName != p.packageName {
		return entitlements.Event{}, fmt.Errorf("%w: %q", ErrPackageMismatch, dn.PackageName)
	}

	ev := entitlements.Event{
		ID:         env.Message.MessageID,
		Source:     entitlements.SourceGoogle,
		OccurredAt: dn.EventTimeMillis.Time(),
		Raw:        bytes.Clone(body),
	}

	switch {
	case dn.TestNotification != nil:
		return entitlements.Event{}, entitlements.ErrIgnored
	case dn.VoidedPurchaseNotification != nil:
		return fromVoided(ev, dn.VoidedPurchaseNotification)
	case dn.SubscriptionNotification != nil:
		return p.fromSubscription(ctx, ev, dn)
	default:
		return entitlements.Event{}, fmt.Errorf("%w: no known notification body", entitlements.ErrUnsupported)
	}
}

// Voided purchase product and refund types (RTDN reference). A zero value
// (field absent) is treated as the common case: full refund of a subscription.
const (
	productTypeOneTime     = 2
	refundTypePartialByQty = 2
)

// fromVoided maps a voidedPurchaseNotification. One-time products are not
// subscriptions and quantity-based partial refunds leave part of the purchase
// valid, so both are ErrIgnored; only a full void of a subscription refunds
// the chain.
// See https://developer.android.com/google/play/billing/rtdn-reference.
func fromVoided(ev entitlements.Event, v *VoidedPurchaseNotification) (entitlements.Event, error) {
	if v.ProductType == productTypeOneTime {
		return entitlements.Event{}, fmt.Errorf("%w: voided one-time product", entitlements.ErrIgnored)
	}
	if v.RefundType == refundTypePartialByQty {
		return entitlements.Event{}, fmt.Errorf("%w: quantity-based partial refund", entitlements.ErrIgnored)
	}
	if v.PurchaseToken == "" {
		return entitlements.Event{}, fmt.Errorf("%w: voided notification without purchaseToken", ErrMalformed)
	}
	ev.OriginalTxID = v.PurchaseToken
	ev.State = entitlements.StateRefunded
	return ev, nil
}

func (p *Parser) fromSubscription(ctx context.Context, ev entitlements.Event, dn DeveloperNotification) (entitlements.Event, error) {
	sn := dn.SubscriptionNotification
	if sn.PurchaseToken == "" {
		return entitlements.Event{}, fmt.Errorf("%w: empty purchaseToken", ErrMalformed)
	}
	fctx, cancel := context.WithTimeout(ctx, p.fetchTO)
	defer cancel()
	sub, err := p.fetcher.GetV2(fctx, dn.PackageName, sn.PurchaseToken)
	if err != nil {
		return entitlements.Event{}, classifyFetchError(err)
	}
	if sub == nil {
		return entitlements.Event{}, errors.New("google: fetcher returned no subscription")
	}
	if sub.TestPurchase && p.rejectTests {
		return entitlements.Event{}, ErrTestPurchase
	}

	state, err := mapState(sub.SubscriptionState)
	if err != nil {
		return entitlements.Event{}, err
	}
	ev.State = state
	ev.OriginalTxID = sn.PurchaseToken
	ev.SupersedesTxID = sub.LinkedPurchaseToken
	ev.ProductID = sn.SubscriptionID
	if ids := sub.ExternalAccountIdentifiers; ids != nil {
		ev.UserID = ids.ObfuscatedExternalAccountID
		if ev.UserID == "" {
			ev.UserID = ids.ObfuscatedExternalProfileID
		}
	}
	applyLineItems(&ev, sub.LineItems)

	switch state {
	case entitlements.StateGrace:
		// The reference only says expiryTime is "the time at which the
		// subscription expired or will expire unless the access is extended"
		// and has no grace end field, so in grace it may already be in the
		// past. Google keeps access on during grace, so open a window of
		// p.grace from the notification time. Google's EXPIRED / ON_HOLD
		// notification closes it early.
		if floor := ev.OccurredAt.Add(p.grace); ev.ExpiresAt.Before(floor) {
			ev.ExpiresAt = floor
		}
	case entitlements.StateActive:
		if ev.ExpiresAt.IsZero() {
			return entitlements.Event{}, ErrNoExpiry
		}
	default:
		// Other states do not grant access; no expiry needed.
	}
	ev.Sandbox = sub.TestPurchase
	return ev, nil
}

// applyLineItems sets ProductID and ExpiresAt from the line item with the
// latest expiry. Items without expiryTime (a deferred replacement that has not
// taken effect yet) never win; if none has one, ProductID falls back to the
// first item.
func applyLineItems(ev *entitlements.Event, items []LineItem) {
	var best *LineItem
	for i := range items {
		li := &items[i]
		if li.ExpiryTime.IsZero() {
			continue
		}
		if best == nil || li.ExpiryTime.After(best.ExpiryTime) {
			best = li
		}
	}
	switch {
	case best != nil:
		ev.ExpiresAt = best.ExpiryTime
		if best.ProductID != "" {
			ev.ProductID = best.ProductID
		}
	case len(items) > 0 && items[0].ProductID != "":
		ev.ProductID = items[0].ProductID
	}
}

// classifyFetchError makes sure a fetch failure carries one of the three
// classes. A fetcher (see package androidpublisher) normally classifies
// itself; anything unclassified, including timeouts, is transient. The
// original error stays reachable through errors.Is/As.
func classifyFetchError(err error) error {
	if errors.Is(err, ErrPurchaseNotFound) || errors.Is(err, ErrFetchConfig) || errors.Is(err, ErrFetchTransient) {
		return fmt.Errorf("google: fetch subscription: %w", err)
	}
	return fmt.Errorf("google: fetch subscription: %w: %w", ErrFetchTransient, err)
}

func mapState(s string) (entitlements.State, error) {
	switch s {
	case StateActive, StateCanceled: // canceled stays active until expiry
		return entitlements.StateActive, nil
	case StateInGracePeriod:
		return entitlements.StateGrace, nil
	case StateOnHold:
		return entitlements.StateBillingRetry, nil
	case StatePaused:
		return entitlements.StatePaused, nil
	case StateExpired, StatePendingCanceled:
		return entitlements.StateExpired, nil
	case StatePending: // awaiting first payment: nothing to grant yet
		return "", entitlements.ErrIgnored
	default:
		return "", fmt.Errorf("%w: subscription state %q", entitlements.ErrUnsupported, s)
	}
}
