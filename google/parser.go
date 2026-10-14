package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tggo/go-entitlements"
)

var (
	// ErrMalformed means the push body or its payload could not be decoded.
	ErrMalformed = errors.New("google: malformed notification")
	// ErrPackageMismatch means the notification is for another application.
	ErrPackageMismatch = errors.New("google: package name mismatch")
)

// Parser turns RTDN Pub/Sub push bodies into entitlements.Event.
type Parser struct {
	fetcher     SubscriptionsFetcher
	packageName string
}

// Option configures a Parser.
type Option func(*Parser)

// WithPackageName rejects notifications for any other packageName.
func WithPackageName(name string) Option {
	return func(p *Parser) { p.packageName = name }
}

// NewParser builds a Parser. fetcher is used for subscription notifications.
func NewParser(fetcher SubscriptionsFetcher, opts ...Option) *Parser {
	p := &Parser{fetcher: fetcher}
	for _, o := range opts {
		o(p)
	}
	return p
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
		Raw:        body,
	}

	switch {
	case dn.TestNotification != nil:
		return entitlements.Event{}, entitlements.ErrIgnored
	case dn.VoidedPurchaseNotification != nil:
		ev.OriginalTxID = dn.VoidedPurchaseNotification.PurchaseToken
		ev.State = entitlements.StateRefunded
		return ev, nil
	case dn.SubscriptionNotification != nil:
		return p.fromSubscription(ctx, ev, dn)
	default:
		return entitlements.Event{}, fmt.Errorf("%w: no known notification body", entitlements.ErrUnsupported)
	}
}

func (p *Parser) fromSubscription(ctx context.Context, ev entitlements.Event, dn DeveloperNotification) (entitlements.Event, error) {
	sn := dn.SubscriptionNotification
	if sn.PurchaseToken == "" {
		return entitlements.Event{}, fmt.Errorf("%w: empty purchaseToken", ErrMalformed)
	}
	sub, err := p.fetcher.GetV2(ctx, dn.PackageName, sn.PurchaseToken)
	if err != nil {
		return entitlements.Event{}, fmt.Errorf("google: fetch subscription: %w", err)
	}
	if sub == nil {
		return entitlements.Event{}, errors.New("google: fetcher returned no subscription")
	}

	state, err := mapState(sub.SubscriptionState)
	if err != nil {
		return entitlements.Event{}, err
	}
	ev.State = state
	ev.OriginalTxID = sn.PurchaseToken
	ev.SupersedesTxID = sub.LinkedPurchaseToken
	ev.ProductID = sn.SubscriptionID
	if sub.ExternalAccountIdentifiers != nil {
		ev.UserID = sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountID
	}
	// Latest expiry wins; its product is reported.
	var best *LineItem
	for i := range sub.LineItems {
		if best == nil || sub.LineItems[i].ExpiryTime.After(best.ExpiryTime) {
			best = &sub.LineItems[i]
		}
	}
	if best != nil {
		ev.ExpiresAt = best.ExpiryTime
		if best.ProductID != "" {
			ev.ProductID = best.ProductID
		}
	}
	return ev, nil
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
