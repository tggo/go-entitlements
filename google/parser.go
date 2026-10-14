package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/tggo/go-entitlements"
)

var (
	// ErrFetcherRequired is returned by NewParser for a nil fetcher.
	ErrFetcherRequired = errors.New("google: NewParser: fetcher is required")
	// ErrPackageRequired is returned by NewParser for an empty package name.
	ErrPackageRequired = errors.New("google: NewParser: package name is required")
	// ErrNoSubscription means the fetcher returned neither a subscription nor
	// an error. It is a fetcher bug; the push is answered 500.
	ErrNoSubscription = errors.New("google: fetcher returned no subscription")
	// ErrMalformed means the push body or its payload could not be decoded.
	ErrMalformed = errors.New("google: malformed notification")
	// ErrPackageMismatch means the notification is for another application.
	ErrPackageMismatch = errors.New("google: package name mismatch")
	// ErrNoExpiry means an access-granting subscription (active, canceled or
	// in grace) came back with no line item expiryTime. Granting it would give
	// open-ended access, so the event is refused (it wraps
	// entitlements.ErrUnsupported). The next notification repairs the state.
	ErrNoExpiry = fmt.Errorf("google: granting subscription has no expiryTime: %w", entitlements.ErrUnsupported)
	// ErrTestNotification is returned for the Play Console "send test"
	// notification (it wraps entitlements.ErrIgnored). It carries no state and
	// is routine, so transports should log it at debug level.
	ErrTestNotification = fmt.Errorf("google: test notification: %w", entitlements.ErrIgnored)
	// ErrTestPurchase is returned for license-tester purchases when
	// WithRejectTestPurchases is set (it wraps entitlements.ErrIgnored).
	ErrTestPurchase = fmt.Errorf("google: test purchase: %w", entitlements.ErrIgnored)

	// ErrPurchaseNotFound means subscriptionsv2.get answered 404 or 410. The token is
	// unknown or too old; redelivery cannot fix it, so acknowledge (HTTP 200).
	ErrPurchaseNotFound = errors.New("google: purchase token not found or expired")
	// ErrPurchaseInvalid means subscriptionsv2.get answered 400: the purchase
	// token is syntactically invalid. Redelivery cannot fix it, so acknowledge
	// (HTTP 200) and log loudly.
	ErrPurchaseInvalid = errors.New("google: purchase token rejected as invalid")
	// ErrFetchConfig means the Play API answered 401 or 403. The service account or
	// its Play Console permissions are misconfigured; retry will not help until
	// an operator fixes it. Answer 500 and log loudly.
	ErrFetchConfig = errors.New("google: play api credentials or permissions rejected")
	// ErrFetchTransient means 5xx, 429, timeouts and transport failures. Answer 500
	// so Pub/Sub redelivers.
	ErrFetchTransient = errors.New("google: play api transient failure")
)

// DefaultMaxFutureSkew is how far ahead of the local clock an
// eventTimeMillis may be before the notification is rejected.
const DefaultMaxFutureSkew = 5 * time.Minute

// DefaultFetchTimeout bounds one subscriptionsv2.get call.
const DefaultFetchTimeout = 15 * time.Second

// Parser turns RTDN Pub/Sub push bodies into entitlements.Event. It is safe
// for concurrent use.
type Parser struct {
	fetcher     SubscriptionsFetcher
	packageName string
	rejectTests bool
	fetchTO     time.Duration
	maxSkew     time.Duration
	now         func() time.Time
}

// Option configures a Parser.
type Option func(*Parser)

// WithRejectTestPurchases makes the parser return ErrTestPurchase for
// license-tester purchases. Default: they are parsed like real ones and
// flagged with Environment EnvironmentGoogleTest, so QA keeps working on production builds.
func WithRejectTestPurchases() Option {
	return func(p *Parser) { p.rejectTests = true }
}

// WithTimeFn injects the clock used to reject notifications dated in the
// future. A nil fn keeps time.Now.
func WithTimeFn(fn func() time.Time) Option {
	return func(p *Parser) {
		if fn != nil {
			p.now = fn
		}
	}
}

// WithMaxFutureSkew sets how far ahead of the clock an eventTimeMillis may be
// (default DefaultMaxFutureSkew). A forged far-future time would otherwise
// become the chain's ordering key and make every genuine later event stale.
// Non-positive values keep the default.
func WithMaxFutureSkew(d time.Duration) Option {
	return func(p *Parser) {
		if d > 0 {
			p.maxSkew = d
		}
	}
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

// NewParser builds a Parser. fetcher is used to read the real subscription
// state and must not be nil. packageName is the application the parser serves
// and is required: notifications for any other package are rejected before
// anything is fetched, so an attacker cannot make the service account query
// other apps. Nil options are ignored.
func NewParser(fetcher SubscriptionsFetcher, packageName string, opts ...Option) (*Parser, error) {
	if fetcher == nil {
		return nil, ErrFetcherRequired
	}
	if packageName == "" {
		return nil, ErrPackageRequired
	}
	p := &Parser{
		fetcher: fetcher, packageName: packageName,
		fetchTO: DefaultFetchTimeout, maxSkew: DefaultMaxFutureSkew, now: time.Now,
	}
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
	if dn.PackageName != p.packageName {
		return entitlements.Event{}, fmt.Errorf("%w: %q", ErrPackageMismatch, dn.PackageName)
	}
	if err := checkID("messageId", env.Message.MessageID); err != nil {
		return entitlements.Event{}, err
	}
	if t := dn.EventTimeMillis.Time(); t.After(p.now().Add(p.maxSkew)) {
		return entitlements.Event{}, fmt.Errorf("%w: eventTimeMillis %s is in the future", ErrMalformed, t.Format(time.RFC3339))
	}

	ev := entitlements.Event{
		ID:         env.Message.MessageID,
		Source:     entitlements.SourceGoogle,
		OccurredAt: dn.EventTimeMillis.Time(),
		Raw:        bytes.Clone(body),
	}

	switch {
	case dn.TestNotification != nil:
		return entitlements.Event{}, ErrTestNotification
	case dn.VoidedPurchaseNotification != nil:
		return p.fromVoided(ctx, ev, dn)
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
// valid, so both are ErrIgnored. Only a full void of a subscription refunds
// the chain, and only after Google confirms it: the notification is not
// trusted on its own (it may be forged if push auth is off, and a void of an
// old renewal order must not end a running subscription). The parser fetches
// the subscription and checks the voided orderId against the latest successful
// order of its line items. If Google reports no order ids, the void is applied
// only when the subscription no longer grants access.
// See https://developer.android.com/google/play/billing/rtdn-reference.
func (p *Parser) fromVoided(ctx context.Context, ev entitlements.Event, dn DeveloperNotification) (entitlements.Event, error) {
	v := dn.VoidedPurchaseNotification
	if v.ProductType == productTypeOneTime {
		return entitlements.Event{}, fmt.Errorf("%w: voided one-time product", entitlements.ErrIgnored)
	}
	if v.RefundType == refundTypePartialByQty {
		return entitlements.Event{}, fmt.Errorf("%w: quantity-based partial refund", entitlements.ErrIgnored)
	}
	if v.PurchaseToken == "" {
		return entitlements.Event{}, fmt.Errorf("%w: voided notification without purchaseToken", ErrMalformed)
	}
	if err := checkID("purchaseToken", v.PurchaseToken); err != nil {
		return entitlements.Event{}, err
	}
	sub, err := p.fetch(ctx, dn.PackageName, v.PurchaseToken)
	if err != nil {
		return entitlements.Event{}, err
	}
	if !voidConfirmed(sub, v.OrderID) {
		return entitlements.Event{}, fmt.Errorf("%w: voided order is not the latest order of the subscription", entitlements.ErrIgnored)
	}
	ev.OriginalTxID = v.PurchaseToken
	ev.State = entitlements.StateRefunded
	return ev, nil
}

func voidConfirmed(sub *SubscriptionPurchaseV2, orderID string) bool {
	known := false
	for _, li := range sub.LineItems {
		if li.LatestSuccessfulOrderID == "" {
			continue
		}
		known = true
		if li.LatestSuccessfulOrderID == orderID {
			return true
		}
	}
	if known {
		return false
	}
	switch sub.SubscriptionState {
	case StateActive, StateCanceled, StateInGracePeriod:
		return false
	default:
		return true
	}
}

// fetch reads the subscription, bounded by the fetch timeout, and makes sure
// a failure carries one error class and never the purchase token.
func (p *Parser) fetch(ctx context.Context, packageName, token string) (*SubscriptionPurchaseV2, error) {
	fctx, cancel := context.WithTimeout(ctx, p.fetchTO)
	defer cancel()
	sub, err := p.fetcher.GetV2(fctx, packageName, token)
	if err != nil {
		return nil, RedactToken(classifyFetchError(err), token)
	}
	if sub == nil {
		return nil, ErrNoSubscription
	}
	return sub, nil
}

// checkID rejects empty-or-control-character identifiers: NUL makes Postgres
// fail permanently, and other control characters corrupt logs.
func checkID(name, v string) error {
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in %s", ErrMalformed, name)
		}
	}
	return nil
}

func (p *Parser) fromSubscription(ctx context.Context, ev entitlements.Event, dn DeveloperNotification) (entitlements.Event, error) {
	sn := dn.SubscriptionNotification
	if sn.PurchaseToken == "" {
		return entitlements.Event{}, fmt.Errorf("%w: empty purchaseToken", ErrMalformed)
	}
	if err := checkID("purchaseToken", sn.PurchaseToken); err != nil {
		return entitlements.Event{}, err
	}
	sub, err := p.fetch(ctx, dn.PackageName, sn.PurchaseToken)
	if err != nil {
		return entitlements.Event{}, err
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
	ev.ProductID = sn.SubscriptionID
	if ids := sub.ExternalAccountIdentifiers; ids != nil {
		ev.UserID = ids.ObfuscatedExternalAccountID
		if ev.UserID == "" {
			ev.UserID = ids.ObfuscatedExternalProfileID
		}
	}
	applyLineItems(&ev, sub.LineItems)

	switch state {
	case entitlements.StateGrace, entitlements.StateActive:
		// expiryTime is the end of paid time, and in grace Play extends it to
		// the end of the grace period
		// (https://developer.android.com/google/play/billing/lifecycle/subscriptions),
		// so it is used as is. Without it the state would grant open-ended access.
		if ev.ExpiresAt.IsZero() {
			return entitlements.Event{}, ErrNoExpiry
		}
		// Only a purchase that grants access retires the old token. A canceled
		// pending upgrade (or an expired/held link) must leave the old chain alone.
		ev.SupersedesTxID = sub.LinkedPurchaseToken
	default:
		// Other states do not grant access; no expiry needed.
	}
	if sub.TestPurchase {
		ev.Environment = entitlements.EnvironmentGoogleTest
	}
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
	if errors.Is(err, ErrPurchaseNotFound) || errors.Is(err, ErrPurchaseInvalid) ||
		errors.Is(err, ErrFetchConfig) || errors.Is(err, ErrFetchTransient) {
		return fmt.Errorf("google: fetch subscription: %w", err)
	}
	return fmt.Errorf("google: fetch subscription: %w: %w", ErrFetchTransient, err)
}

func mapState(s State) (entitlements.State, error) {
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

// RedactToken returns err with every occurrence of the purchase token in its
// text replaced by "[redacted]". The original stays reachable through
// errors.Is/As. Purchase tokens are bearer-like secrets for the Play API; the
// URL of a failed request contains one.
func RedactToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return &redacted{msg: strings.ReplaceAll(err.Error(), token, "[redacted]"), err: err}
}

type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.err }
