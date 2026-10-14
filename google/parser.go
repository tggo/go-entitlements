package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// ErrMultipleEvents is returned by Parse when the notification needs more
	// than one event (add-ons, or an upgrade retiring several tokens). Use
	// ParseAll.
	ErrMultipleEvents = errors.New("google: notification yields several events, use ParseAll")
	// ErrAlreadyAcknowledged means the Play API answered 409 to an acknowledge
	// request: the purchase is already acknowledged. It is a success for our
	// purposes; acknowledging is idempotent.
	ErrAlreadyAcknowledged = errors.New("google: purchase already acknowledged")
	// ErrFetchConfig means the Play API answered 401 or 403. The service account or
	// its Play Console permissions are misconfigured; retry will not help until
	// an operator fixes it. Answer 500 and log loudly.
	ErrFetchConfig = errors.New("google: play api credentials or permissions rejected")
	// ErrFetchTransient means 5xx, 429, timeouts and transport failures. Answer 500
	// so Pub/Sub redelivers.
	ErrFetchTransient = errors.New("google: play api transient failure")
)

// ackPending is subscriptionsv2 acknowledgementState for a purchase that has
// not been acknowledged yet.
const ackPending = "ACKNOWLEDGEMENT_STATE_PENDING"

// DefaultMaxFutureSkew is how far ahead of the local clock an
// eventTimeMillis may be before the notification is rejected.
const DefaultMaxFutureSkew = 5 * time.Minute

// DefaultFetchTimeout bounds one subscriptionsv2.get call.
const DefaultFetchTimeout = 15 * time.Second

// Ack describes a purchase that still has to be acknowledged.
type Ack struct {
	PackageName    string
	SubscriptionID string
	PurchaseToken  string
}

// Result is a parsed notification: the events to apply and the
// acknowledgement the purchase needs.
type Result struct {
	// Events are applied in order. A subscription with add-ons yields one per
	// line item (see ParseAll), plus events retiring the tokens it replaces.
	Events []entitlements.Event
	// Ack is set when Google reports the purchase as not yet acknowledged
	// (ACKNOWLEDGEMENT_STATE_PENDING) and the subscription grants access. It
	// is nil otherwise. Acknowledge only after the Events have been stored.
	Ack *Ack
}

// MaxLinkedTokens bounds how many replaced purchase tokens (linkedPurchaseToken
// chain) the parser follows, one subscriptionsv2.get each, when retiring the
// chain of an upgrade.
const MaxLinkedTokens = 4

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

// Parse decodes a Pub/Sub push body and maps it to a single Event. It returns
// ErrMultipleEvents when the purchase needs several (add-ons, or an upgrade
// that retires several tokens): use ParseAll. It returns
// entitlements.ErrIgnored for test notifications (and pending purchases) and
// entitlements.ErrUnsupported for notifications or states it cannot map.
func (p *Parser) Parse(ctx context.Context, body []byte) (entitlements.Event, error) {
	evs, err := p.ParseAll(ctx, body)
	if err != nil {
		return entitlements.Event{}, err
	}
	if len(evs) != 1 {
		return entitlements.Event{}, fmt.Errorf("%w: %d events", ErrMultipleEvents, len(evs))
	}
	return evs[0], nil
}

// ParseAll decodes a push and returns every event it implies, to be applied in
// order. A subscription with add-ons has one line item per entitlement, each
// with its own expiryTime
// (https://developer.android.com/google/play/billing/subscription-with-addons),
// so each becomes its own Event:
//
//   - Key: a subscription that yields exactly one item keeps OriginalTxID =
//     purchaseToken and ID = messageId, as before add-ons were supported, so
//     stored rows stay valid. With several items each uses OriginalTxID =
//     purchaseToken + "#" + productId and ID = messageId + "#" + productId.
//     A product-based suffix (not a position) keeps a row bound to its
//     product when items are added, removed or reordered.
//   - Items without expiryTime (a deferred replacement that has not taken
//     effect) grant nothing and yield no event. With several items, an
//     active item whose expiryTime has passed (a removed add-on "expires at
//     the end of its current billing period") yields none either: its stored
//     row already ends at that time.
//   - Upgrades: the replaced tokens are retired; see the package comment.
//   - A voided purchase refunds every item key and the plain token key.
//
// Rows written under a different key shape (a single-item subscription that
// later gains an add-on, or the reverse) are not rewritten; they end at their
// own stored expiryTime.
func (p *Parser) ParseAll(ctx context.Context, body []byte) ([]entitlements.Event, error) {
	res, err := p.ParseResult(ctx, body)
	return res.Events, err
}

// ParseResult is ParseAll plus the acknowledgement the purchase needs (see Ack).
func (p *Parser) ParseResult(ctx context.Context, body []byte) (Result, error) {
	var env PushEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Result{}, fmt.Errorf("%w: envelope: %w", ErrMalformed, err)
	}
	if env.Message.MessageID == "" || len(env.Message.Data) == 0 {
		return Result{}, fmt.Errorf("%w: missing messageId or data", ErrMalformed)
	}
	var dn DeveloperNotification
	if err := json.Unmarshal(env.Message.Data, &dn); err != nil {
		return Result{}, fmt.Errorf("%w: payload: %w", ErrMalformed, err)
	}
	if dn.PackageName != p.packageName {
		return Result{}, fmt.Errorf("%w: %q", ErrPackageMismatch, dn.PackageName)
	}
	if err := checkID("messageId", env.Message.MessageID); err != nil {
		return Result{}, err
	}
	if t := dn.EventTimeMillis.Time(); t.After(p.now().Add(p.maxSkew)) {
		return Result{}, fmt.Errorf("%w: eventTimeMillis %s is in the future", ErrMalformed, t.Format(time.RFC3339))
	}

	base := entitlements.Event{
		ID:         env.Message.MessageID,
		Source:     entitlements.SourceGoogle,
		OccurredAt: dn.EventTimeMillis.Time(),
		Raw:        bytes.Clone(body),
	}

	switch {
	case dn.TestNotification != nil:
		return Result{}, ErrTestNotification
	case dn.VoidedPurchaseNotification != nil:
		return p.fromVoided(ctx, base, dn)
	case dn.SubscriptionNotification != nil:
		sn := dn.SubscriptionNotification
		return p.fromSubscription(ctx, base, subscriptionRef{packageName: dn.PackageName, subscriptionID: sn.SubscriptionID, token: sn.PurchaseToken}, false)
	default:
		return Result{}, fmt.Errorf("%w: no known notification body", entitlements.ErrUnsupported)
	}
}

// ParseToken maps the current state of a purchase token the app sends right
// after a purchase, without waiting for the push. subscriptionID may be empty
// (Play no longer requires it); it is only used as a fallback product id and
// for the acknowledge path.
//
// The events are deterministic for a given state: ID is a digest of the
// token's state and items, so repeating the sync (or racing it with a push of
// the same state) dedupes, while a changed state is a new event. OccurredAt is
// the fetch time minus the clock-skew margin (WithMaxFutureSkew). That makes a
// sync unable to overwrite a push that is newer than the state it fetched: the
// store drops an event older than what it has applied. A push older than the
// margin describes a state the fetch already includes, so the sync rightly
// replaces it, and any later change arrives with a newer push that applies on
// top. The purchase token never appears in an event ID.
func (p *Parser) ParseToken(ctx context.Context, subscriptionID, purchaseToken string) (Result, error) {
	if purchaseToken == "" {
		return Result{}, fmt.Errorf("%w: empty purchaseToken", ErrMalformed)
	}
	if err := checkID("purchaseToken", purchaseToken); err != nil {
		return Result{}, err
	}
	if err := checkID("subscriptionId", subscriptionID); err != nil {
		return Result{}, err
	}
	base := entitlements.Event{
		Source:     entitlements.SourceGoogle,
		OccurredAt: p.now().Add(-p.maxSkew).UTC(),
	}
	return p.fromSubscription(ctx, base, subscriptionRef{packageName: p.packageName, subscriptionID: subscriptionID, token: purchaseToken}, true)
}

type subscriptionRef struct {
	packageName, subscriptionID, token string
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
// only when the subscription no longer grants access. Every key of the
// purchase is refunded: the plain token and, with several items, each item.
// See https://developer.android.com/google/play/billing/rtdn-reference.
func (p *Parser) fromVoided(ctx context.Context, base entitlements.Event, dn DeveloperNotification) (Result, error) {
	v := dn.VoidedPurchaseNotification
	if v.ProductType == productTypeOneTime {
		return Result{}, fmt.Errorf("%w: voided one-time product", entitlements.ErrIgnored)
	}
	if v.RefundType == refundTypePartialByQty {
		return Result{}, fmt.Errorf("%w: quantity-based partial refund", entitlements.ErrIgnored)
	}
	if v.PurchaseToken == "" {
		return Result{}, fmt.Errorf("%w: voided notification without purchaseToken", ErrMalformed)
	}
	if err := checkID("purchaseToken", v.PurchaseToken); err != nil {
		return Result{}, err
	}
	sub, err := p.fetch(ctx, dn.PackageName, v.PurchaseToken)
	if err != nil {
		return Result{}, err
	}
	if !voidConfirmed(sub, v.OrderID) {
		return Result{}, fmt.Errorf("%w: voided order is not the latest order of the subscription", entitlements.ErrIgnored)
	}
	var evs []entitlements.Event
	for i, key := range tokenKeys(v.PurchaseToken, sub.LineItems) {
		ev := base
		ev.OriginalTxID = key.key
		ev.State = entitlements.StateRefunded
		if i > 0 {
			ev.ID = base.ID + key.idSuffix
		}
		evs = append(evs, ev)
	}
	return Result{Events: evs}, nil
}

type itemKey struct{ key, idSuffix string }

// tokenKeys lists every OriginalTxID a purchase token may be stored under: the
// plain token and, when it has several items, one per product. The plain key
// comes first and has an empty id suffix.
func tokenKeys(token string, items []LineItem) []itemKey {
	keys := []itemKey{{key: token}}
	if len(items) <= 1 {
		return keys
	}
	for i, li := range items {
		suffix := "#" + li.ProductID
		if li.ProductID == "" {
			suffix = fmt.Sprintf("#%d", i)
		}
		keys = append(keys, itemKey{key: token + suffix, idSuffix: suffix})
	}
	return keys
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

// checkID rejects control-character identifiers: NUL makes Postgres fail
// permanently, and other control characters corrupt logs.
func checkID(name, v string) error {
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in %s", ErrMalformed, name)
		}
	}
	return nil
}

func (p *Parser) fromSubscription(ctx context.Context, base entitlements.Event, ref subscriptionRef, sync bool) (Result, error) {
	if ref.token == "" {
		return Result{}, fmt.Errorf("%w: empty purchaseToken", ErrMalformed)
	}
	if err := checkID("purchaseToken", ref.token); err != nil {
		return Result{}, err
	}
	sub, err := p.fetch(ctx, ref.packageName, ref.token)
	if err != nil {
		return Result{}, err
	}
	if sub.TestPurchase && p.rejectTests {
		return Result{}, ErrTestPurchase
	}
	state, err := mapState(sub.SubscriptionState)
	if err != nil {
		return Result{}, err
	}

	items, err := p.selectItems(sub, state, ref.subscriptionID)
	if err != nil {
		return Result{}, err
	}
	if sync {
		base.ID = syncID(sub, items)
	}
	user := ""
	if ids := sub.ExternalAccountIdentifiers; ids != nil {
		user = ids.ObfuscatedExternalAccountID
		if user == "" {
			user = ids.ObfuscatedExternalProfileID
		}
	}

	var evs []entitlements.Event
	for _, k := range itemKeys(ref.token, items) {
		ev := base
		ev.ID = base.ID + k.idSuffix
		ev.OriginalTxID = k.key
		ev.State = state
		ev.ProductID = k.item.ProductID
		ev.ExpiresAt = k.item.ExpiryTime
		ev.UserID = user
		if plan := k.item.AutoRenewingPlan; plan != nil {
			on := plan.AutoRenewEnabled
			ev.AutoRenew = &on
		}
		if sub.TestPurchase {
			ev.Environment = entitlements.EnvironmentGoogleTest
		}
		evs = append(evs, ev)
	}

	// Only a purchase that grants access retires the replaced tokens. A
	// canceled pending upgrade (or an expired/held link) must leave the old
	// chain alone.
	if grants(state) && sub.LinkedPurchaseToken != "" && sub.LinkedPurchaseToken != ref.token {
		retire, err := p.retireLinked(ctx, base, ref, sub.LinkedPurchaseToken)
		if err != nil {
			return Result{}, err
		}
		evs[0].SupersedesTxID = sub.LinkedPurchaseToken
		evs = append(evs, retire...)
	}

	res := Result{Events: evs}
	if sub.AcknowledgementState == ackPending && grants(state) {
		id := ref.subscriptionID
		if id == "" {
			id = items[0].ProductID
		}
		res.Ack = &Ack{PackageName: ref.packageName, SubscriptionID: id, PurchaseToken: ref.token}
	}
	return res, nil
}

func grants(s entitlements.State) bool {
	return s == entitlements.StateActive || s == entitlements.StateGrace
}

// selectItems picks the line items that become events (see ParseAll). It
// returns at least one item. A subscription with no line items yields a single
// empty item whose product is the notification's subscriptionId. For
// access-granting states an item must carry an expiryTime, because without one
// the state would grant open-ended access: if none qualifies, ErrNoExpiry.
// In grace Play extends expiryTime to the end of the grace period
// (https://developer.android.com/google/play/billing/lifecycle/subscriptions),
// so it is used as is.
func (p *Parser) selectItems(sub *SubscriptionPurchaseV2, state entitlements.State, subscriptionID string) ([]LineItem, error) {
	items := sub.LineItems
	if !grants(state) {
		if len(items) == 0 {
			return []LineItem{{ProductID: subscriptionID}}, nil
		}
		out := slices.Clone(items)
		fillProduct(out, subscriptionID)
		return out, nil
	}
	multi := len(items) > 1
	now := p.now()
	var out []LineItem
	for _, li := range items {
		switch {
		case li.ExpiryTime.IsZero():
			continue // pending deferred replacement: grants nothing yet
		case multi && state == entitlements.StateActive && !li.ExpiryTime.After(now):
			continue // lapsed add-on: its row already ends at this time
		}
		out = append(out, li)
	}
	if len(out) == 0 {
		return nil, ErrNoExpiry
	}
	fillProduct(out, subscriptionID)
	return out, nil
}

// fillProduct gives a single item without a product id the notification's
// subscriptionId.
func fillProduct(items []LineItem, subscriptionID string) {
	if len(items) == 1 && items[0].ProductID == "" {
		items[0].ProductID = subscriptionID
	}
}

type keyedItem struct {
	itemKey
	item LineItem
}

// itemKeys assigns each selected item its OriginalTxID and id suffix: the
// plain token for a single item, token#productId for several.
func itemKeys(token string, items []LineItem) []keyedItem {
	if len(items) == 1 {
		return []keyedItem{{itemKey{key: token}, items[0]}}
	}
	out := make([]keyedItem, 0, len(items))
	seen := map[string]int{}
	for i, li := range items {
		suffix := "#" + li.ProductID
		if li.ProductID == "" {
			suffix = fmt.Sprintf("#%d", i)
		}
		if n := seen[suffix]; n > 0 { // the same product twice in one purchase
			suffix = fmt.Sprintf("%s#%d", suffix, n+1)
		}
		seen[suffix]++
		out = append(out, keyedItem{itemKey{key: token + suffix, idSuffix: suffix}, li})
	}
	return out
}

// syncID is the deterministic id of a ParseToken event: a digest of the state
// and items, so identical state dedupes and changed state does not. The token
// is deliberately not part of the id text.
func syncID(sub *SubscriptionPurchaseV2, items []LineItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s", sub.SubscriptionState, sub.LinkedPurchaseToken, sub.AcknowledgementState)
	for _, li := range items {
		fmt.Fprintf(h, "|%s@%d", li.ProductID, li.ExpiryTime.UnixMilli())
	}
	return "sync:" + hex.EncodeToString(h.Sum(nil))
}

// retireLinked follows linkedPurchaseToken (at most MaxLinkedTokens
// subscriptionsv2.get calls) and returns events retiring every key of the
// replaced tokens except the plain key of the first one, which the caller
// retires through Event.SupersedesTxID (that path also hands the old chain's
// user to a new token that has none). A replaced token Google no longer knows
// (404, 410, 400) is retired by its plain key only; a transient or
// configuration failure is returned, so the push is retried.
func (p *Parser) retireLinked(ctx context.Context, base entitlements.Event, ref subscriptionRef, linked string) ([]entitlements.Event, error) {
	var evs []entitlements.Event
	seen := map[string]bool{ref.token: true}
	for depth := 0; linked != "" && !seen[linked] && depth < MaxLinkedTokens; depth++ {
		seen[linked] = true
		old, err := p.fetch(ctx, ref.packageName, linked)
		var keys []itemKey
		next := ""
		switch {
		case err == nil:
			keys = tokenKeys(linked, old.LineItems)
			next = old.LinkedPurchaseToken
		case errors.Is(err, ErrPurchaseNotFound), errors.Is(err, ErrPurchaseInvalid):
			keys = tokenKeys(linked, nil)
		default:
			return nil, err
		}
		for i, k := range keys {
			if depth == 0 && i == 0 {
				continue // the supersede link
			}
			ev := base
			ev.ID = retireID(base.ID, k.key)
			ev.OriginalTxID = k.key
			ev.State = entitlements.StateExpired
			evs = append(evs, ev)
		}
		linked = next
	}
	return evs, nil
}

func retireID(baseID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return baseID + "#retire#" + hex.EncodeToString(sum[:8])
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
