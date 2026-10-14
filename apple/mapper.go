package apple

import (
	"errors"
	"fmt"
	"time"

	"github.com/tggo/go-entitlements"
)

// ErrNoExpiry means an access-granting event came without the date it needs
// (expiresDate, or gracePeriodExpiresDate for a grace period). Granting it
// would give open-ended access or silently deny it, so the event is refused
// (it wraps entitlements.ErrUnsupported). The next notification repairs the
// state.
var ErrNoExpiry = fmt.Errorf("apple: granting event has no expiry date: %w", entitlements.ErrUnsupported)

const autoRenewable = "Auto-Renewable Subscription"

// ToEvent maps a verified notification to a normalized event.
//
// Identity and time:
//   - ID = notificationUUID, OriginalTxID = originalTransactionId,
//     TransactionID = transactionId (the billing period a REFUND names),
//     OccurredAt = signedDate of the outer payload, ExpiresAt = expiresDate.
//   - UserID = appAccountToken when the app sets one, otherwise empty: many
//     apps link users to originalTransactionId themselves, so the caller
//     resolves the user from Event.OriginalTxID (the real key).
//   - Environment = the transaction's environment (Production, Sandbox, ...).
//
// State:
//   - SUBSCRIBED, DID_RENEW → active. Together with REFUND_REVERSED they are
//     the only events that may lift a refunded or revoked chain.
//   - DID_FAIL_TO_RENEW+GRACE_PERIOD → grace until gracePeriodExpiresDate
//     (required, else ErrNoExpiry); DID_FAIL_TO_RENEW and GRACE_PERIOD_EXPIRED
//     → billing_retry; EXPIRED → expired; REFUND → refunded; REVOKE → revoked.
//   - DID_CHANGE_RENEWAL_STATUS, DID_CHANGE_RENEWAL_PREF, OFFER_REDEEMED,
//     RENEWAL_EXTENDED and REFUND_REVERSED do not say what the state is, so it
//     is read from data.status (1 active, 2 expired, 3 billing retry,
//     4 grace, 5 revoked). Without a status it falls back to comparing
//     expiresDate with signedDate. Except REFUND_REVERSED they are marked
//     NoRevive, so an AUTO_RENEW_DISABLED sent after a refund cannot undo it.
//   - A transaction with revocationDate is refunded whatever the type says
//     (REVOKE → revoked, REFUND_REVERSED restores).
//   - REFUND_REVERSED sets Restores. REFUND carries the refunded
//     transaction's id and expiresDate; Apply records it, ends the chain only
//     if that transaction is the one currently granting access, and lets a
//     renewal that arrives after the refund (a different transaction) revive
//     the chain even if it was signed earlier.
//   - Active and grace events need an expiry: without one they return
//     ErrNoExpiry.
//
// Not state:
//   - TEST, ONE_TIME_CHARGE, RENEWAL_EXTENSION, REFUND_DECLINED,
//     PRICE_INCREASE, PRICE_CHANGE, CONSUMPTION_REQUEST, METADATA_UPDATE,
//     MIGRATION, EXTERNAL_PURCHASE_TOKEN, RESCIND_CONSENT and
//     CHILD_CONSENT_REVOKED return ErrIgnored. Your app must answer
//     CONSUMPTION_REQUEST itself. REFUND and REVOKE for a transaction that is
//     not an auto-renewable subscription are ignored too.
//   - Any other type returns ErrUnsupported.
//
// Raw is left nil; Parser fills it with the request body.
func ToEvent(n *Notification) (entitlements.Event, error) {
	if n == nil {
		return entitlements.Event{}, errors.New("apple: nil notification")
	}
	p := n.Payload
	if ignoredTypes[p.NotificationType] {
		return entitlements.Event{}, fmt.Errorf("%w: apple %s/%s carries no subscription state", entitlements.ErrIgnored, p.NotificationType, p.Subtype)
	}
	if p.NotificationType == "ONE_TIME_CHARGE" {
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
	if (p.NotificationType == "REFUND" || p.NotificationType == "REVOKE") && tx.Type != "" && tx.Type != autoRenewable {
		return entitlements.Event{}, fmt.Errorf("%w: %s of a %q transaction is not subscription state", entitlements.ErrIgnored, p.NotificationType, tx.Type)
	}

	occurred := msToTime(p.SignedDate)
	expires := msToTime(tx.ExpiresDate)
	status := 0
	if p.Data != nil {
		status = p.Data.Status
	}

	state := m.state
	if m.ambiguous {
		state = resolve(tx, status, expires, occurred, m.restores)
	} else if state == entitlements.StateActive && tx.RevocationDate != 0 {
		state = entitlements.StateRefunded
	}

	switch state {
	case entitlements.StateActive:
		if expires.IsZero() {
			return entitlements.Event{}, fmt.Errorf("%w: %s", ErrNoExpiry, p.NotificationType)
		}
	case entitlements.StateGrace:
		var graceEnd time.Time
		if n.Renewal != nil {
			graceEnd = msToTime(n.Renewal.GracePeriodExpiresDate)
		}
		if graceEnd.IsZero() {
			return entitlements.Event{}, fmt.Errorf("%w: %s in grace without gracePeriodExpiresDate", ErrNoExpiry, p.NotificationType)
		}
		if graceEnd.After(expires) {
			expires = graceEnd
		}
	}

	env := entitlements.Environment(tx.Environment)
	if env == "" && p.Data != nil {
		env = entitlements.Environment(p.Data.Environment)
	}
	return entitlements.Event{
		ID:            p.NotificationUUID,
		Source:        entitlements.SourceApple,
		UserID:        tx.AppAccountToken,
		OriginalTxID:  tx.OriginalTransactionID,
		TransactionID: tx.TransactionID,
		ProductID:     tx.ProductID,
		State:         state,
		ExpiresAt:     expires,
		OccurredAt:    occurred,
		Restores:      m.restores,
		NoRevive:      m.ambiguous && !m.restores,
		Environment:   env,
		AutoRenew:     autoRenew(n),
	}, nil
}

// resolve reads the state of an event whose type does not name one.
func resolve(tx *JWSTransactionDecodedPayload, status int, expires, signed time.Time, restores bool) entitlements.State {
	if tx.RevocationDate != 0 && !restores {
		return entitlements.StateRefunded
	}
	switch status {
	case 1:
		return entitlements.StateActive
	case 2:
		return entitlements.StateExpired
	case 3:
		return entitlements.StateBillingRetry
	case 4:
		return entitlements.StateGrace
	case 5:
		return entitlements.StateRevoked
	}
	if expires.After(signed) {
		return entitlements.StateActive
	}
	return entitlements.StateExpired
}

// ignoredTypes are known notification types that carry no subscription state.
var ignoredTypes = map[string]bool{
	"TEST": true, "RENEWAL_EXTENSION": true, "REFUND_DECLINED": true, "PRICE_INCREASE": true,
	"PRICE_CHANGE": true, "CONSUMPTION_REQUEST": true, "METADATA_UPDATE": true, "MIGRATION": true,
	"EXTERNAL_PURCHASE_TOKEN": true, "RESCIND_CONSENT": true, "CHILD_CONSENT_REVOKED": true,
}

type mapping struct {
	state     entitlements.State
	ambiguous bool // read the state from data.status / revocationDate / expiry
	restores  bool
}

func mapType(typ, subtype string) (mapping, error) {
	switch typ {
	case "SUBSCRIBED", "DID_RENEW":
		return mapping{state: entitlements.StateActive}, nil
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
	case "DID_CHANGE_RENEWAL_STATUS", "DID_CHANGE_RENEWAL_PREF", "OFFER_REDEEMED", "RENEWAL_EXTENDED":
		return mapping{ambiguous: true}, nil
	case "REFUND_REVERSED":
		return mapping{ambiguous: true, restores: true}, nil
	}
	return mapping{}, fmt.Errorf("%w: apple notificationType %q subtype %q", entitlements.ErrUnsupported, typ, subtype)
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// autoRenew reads the renewal setting: renewalInfo.autoRenewStatus when the
// notification carries renewal info, else the AUTO_RENEW_ENABLED/DISABLED
// subtype of DID_CHANGE_RENEWAL_STATUS, else unknown (nil).
func autoRenew(n *Notification) *bool {
	var v bool
	switch {
	case n.Renewal != nil && n.Renewal.AutoRenewStatus != nil:
		v = *n.Renewal.AutoRenewStatus == 1
	case n.Payload.NotificationType == "DID_CHANGE_RENEWAL_STATUS" && n.Payload.Subtype == "AUTO_RENEW_ENABLED":
		v = true
	case n.Payload.NotificationType == "DID_CHANGE_RENEWAL_STATUS" && n.Payload.Subtype == "AUTO_RENEW_DISABLED":
		v = false
	default:
		return nil
	}
	return &v
}
