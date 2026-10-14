package apple

import (
	"errors"
	"fmt"
	"time"

	"github.com/tggo/go-entitlements"
)

// ToEvent maps a verified notification to a normalized event.
//
//   - TEST returns entitlements.ErrIgnored; unmapped types return ErrUnsupported.
//   - UserID = appAccountToken when the app sets one, otherwise empty: many
//     apps link users to originalTransactionId themselves, so the caller must
//     resolve the user from Event.OriginalTxID (the real key). Parse has no
//     context, so there is no resolver hook.
//   - REFUND_REVERSED restores the chain (Event.Restores, state from the
//     transaction like DID_CHANGE_RENEWAL_STATUS). RENEWAL_EXTENDED is active
//     with the new expiresDate. RENEWAL_EXTENSION (SUMMARY/FAILURE) is ignored.
//   - REFUND carries the refunded transaction's expiresDate; Apply keeps the
//     chain alive if a newer transaction already runs past it.
//   - Event.Environment is the transaction's environment (Production, Sandbox, ...).
//   - ONE_TIME_CHARGE returns ErrIgnored (non-subscription purchase).
//   - OriginalTxID = originalTransactionId,
//     ExpiresAt = expiresDate, OccurredAt = signedDate (outer payload),
//     ID = notificationUUID.
//   - DID_CHANGE_RENEWAL_STATUS only toggles auto-renew, so the state is
//     derived from the transaction: active if expiresDate is after the
//     notification's signedDate, otherwise expired.
//   - In grace (DID_FAIL_TO_RENEW/GRACE_PERIOD) ExpiresAt is extended to the
//     renewal info's gracePeriodExpiresDate when that is later, so
//     Entitlement.Active keeps granting access during the grace period.
//
// Raw is left nil; Parser fills it with the request body.
func ToEvent(n *Notification) (entitlements.Event, error) {
	if n == nil {
		return entitlements.Event{}, errors.New("apple: nil notification")
	}
	p := n.Payload
	switch p.NotificationType {
	case "RENEWAL_EXTENSION":
		// SUMMARY (bulk result, no per-subscriber data) and FAILURE carry no
		// new expiry; the per-subscriber success is RENEWAL_EXTENDED.
		return entitlements.Event{}, fmt.Errorf("%w: RENEWAL_EXTENSION/%s has no subscription state", entitlements.ErrIgnored, p.Subtype)
	case "TEST":
		return entitlements.Event{}, entitlements.ErrIgnored
	case "ONE_TIME_CHARGE":
		// Consumables, non-consumables and non-renewing subscriptions are not
		// subscription state. Non-renewing subscriptions would need an
		// app-defined duration (Apple sends no expiresDate), so they are
		// ignored too rather than guessed.
		typ := ""
		if n.Transaction != nil {
			typ = n.Transaction.Type
		}
		return entitlements.Event{}, fmt.Errorf("%w: ONE_TIME_CHARGE for %q is a non-subscription purchase, out of scope", entitlements.ErrIgnored, typ)
	}
	m, err := mapType(p.NotificationType, p.Subtype)
	if err != nil {
		return entitlements.Event{}, err
	}
	tx := n.Transaction
	if tx == nil {
		return entitlements.Event{}, fmt.Errorf("apple: %s without signedTransactionInfo", p.NotificationType)
	}
	env := tx.Environment
	if env == "" && p.Data != nil {
		env = p.Data.Environment
	}
	occurred := msToTime(p.SignedDate)
	expires := msToTime(tx.ExpiresDate)
	if m.derive {
		m.state = entitlements.StateExpired
		if expires.After(occurred) {
			m.state = entitlements.StateActive
		}
	}
	if m.state == entitlements.StateGrace && n.Renewal != nil {
		if g := msToTime(n.Renewal.GracePeriodExpiresDate); g.After(expires) {
			expires = g
		}
	}
	return entitlements.Event{
		ID:           p.NotificationUUID,
		Source:       entitlements.SourceApple,
		UserID:       tx.AppAccountToken,
		OriginalTxID: tx.OriginalTransactionID,
		ProductID:    tx.ProductID,
		State:        m.state,
		ExpiresAt:    expires,
		OccurredAt:   occurred,
		Restores:     m.restores,
		Environment:  env,
		Sandbox:      env == "Sandbox" || env == "Xcode" || env == "LocalTesting",
	}, nil
}

type mapping struct {
	state    entitlements.State
	derive   bool // compute active/expired from the transaction's expiresDate vs signedDate
	restores bool
}

func mapType(typ, subtype string) (mapping, error) {
	active := mapping{state: entitlements.StateActive}
	switch typ {
	case "SUBSCRIBED", "DID_RENEW", "OFFER_REDEEMED", "DID_CHANGE_RENEWAL_PREF":
		return active, nil
	case "DID_FAIL_TO_RENEW":
		if subtype == "GRACE_PERIOD" {
			return mapping{state: entitlements.StateGrace}, nil
		}
		return mapping{state: entitlements.StateBillingRetry}, nil
	case "GRACE_PERIOD_EXPIRED":
		return mapping{state: entitlements.StateBillingRetry}, nil
	case "EXPIRED":
		return mapping{state: entitlements.StateExpired}, nil
	case "REFUND":
		// Names one transaction; Apply decides whether it ends the chain.
		return mapping{state: entitlements.StateRefunded}, nil
	case "REVOKE":
		return mapping{state: entitlements.StateRevoked}, nil
	case "DID_CHANGE_RENEWAL_STATUS", "RENEWAL_EXTENDED":
		return mapping{derive: true}, nil
	case "REFUND_REVERSED":
		return mapping{derive: true, restores: true}, nil
	}
	return mapping{}, fmt.Errorf("%w: apple notificationType %q subtype %q", entitlements.ErrUnsupported, typ, subtype)
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
