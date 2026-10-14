package apple

import (
	"errors"
	"fmt"
	"time"

	"github.com/tggo/go-entitlements"
)

// ErrMissingAccountToken means the transaction has no appAccountToken, so the
// event cannot be attributed to a user. It wraps entitlements.ErrUnsupported:
// retrying will not help, so webhooks should acknowledge and log it.
var ErrMissingAccountToken = fmt.Errorf("%w: transaction has no appAccountToken", entitlements.ErrUnsupported)

// ToEvent maps a verified notification to a normalized event.
//
//   - TEST returns entitlements.ErrIgnored; unmapped types return ErrUnsupported.
//   - UserID = appAccountToken, OriginalTxID = originalTransactionId,
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
	if p.NotificationType == "TEST" {
		return entitlements.Event{}, entitlements.ErrIgnored
	}
	state, derive, err := mapState(p.NotificationType, p.Subtype)
	if err != nil {
		return entitlements.Event{}, err
	}
	tx := n.Transaction
	if tx == nil {
		return entitlements.Event{}, fmt.Errorf("apple: %s without signedTransactionInfo", p.NotificationType)
	}
	if tx.AppAccountToken == "" {
		return entitlements.Event{}, ErrMissingAccountToken
	}
	occurred := msToTime(p.SignedDate)
	expires := msToTime(tx.ExpiresDate)
	if derive {
		state = entitlements.StateExpired
		if expires.After(occurred) {
			state = entitlements.StateActive
		}
	}
	if state == entitlements.StateGrace && n.Renewal != nil {
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
		State:        state,
		ExpiresAt:    expires,
		OccurredAt:   occurred,
	}, nil
}

// mapState returns the target state; derive means "compute from the transaction".
func mapState(typ, subtype string) (state entitlements.State, derive bool, err error) {
	switch typ {
	case "SUBSCRIBED", "DID_RENEW", "OFFER_REDEEMED", "DID_CHANGE_RENEWAL_PREF":
		return entitlements.StateActive, false, nil
	case "DID_FAIL_TO_RENEW":
		if subtype == "GRACE_PERIOD" {
			return entitlements.StateGrace, false, nil
		}
		return entitlements.StateBillingRetry, false, nil
	case "GRACE_PERIOD_EXPIRED":
		return entitlements.StateBillingRetry, false, nil
	case "EXPIRED":
		return entitlements.StateExpired, false, nil
	case "REFUND":
		return entitlements.StateRefunded, false, nil
	case "REVOKE":
		return entitlements.StateRevoked, false, nil
	case "DID_CHANGE_RENEWAL_STATUS":
		return "", true, nil
	}
	return "", false, fmt.Errorf("%w: apple notificationType %q subtype %q", entitlements.ErrUnsupported, typ, subtype)
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
