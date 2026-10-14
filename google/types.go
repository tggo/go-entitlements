package google

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// PushEnvelope is the Pub/Sub push request body.
type PushEnvelope struct {
	Message      PushMessage `json:"message"`
	Subscription string      `json:"subscription"`
}

// PushMessage is the Pub/Sub message inside a push envelope.
type PushMessage struct {
	// Data is the base64-decoded payload (encoding/json decodes base64 for []byte).
	Data        []byte            `json:"data"`
	MessageID   string            `json:"messageId"`
	PublishTime string            `json:"publishTime"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Millis is a Unix-millisecond timestamp. RTDN sends it as a JSON string;
// plain numbers are accepted too.
type Millis int64

// UnmarshalJSON implements json.Unmarshaler.
func (m *Millis) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	if s == "" || s == "null" {
		*m = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid millis %q: %w", s, err)
	}
	*m = Millis(v)
	return nil
}

// Time converts to time.Time (UTC); zero stays the zero time.
func (m Millis) Time() time.Time {
	if m == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(m)).UTC()
}

// DeveloperNotification is the RTDN payload carried in the Pub/Sub message data.
type DeveloperNotification struct {
	Version                    string                      `json:"version"`
	PackageName                string                      `json:"packageName"`
	EventTimeMillis            Millis                      `json:"eventTimeMillis"`
	SubscriptionNotification   *SubscriptionNotification   `json:"subscriptionNotification,omitempty"`
	VoidedPurchaseNotification *VoidedPurchaseNotification `json:"voidedPurchaseNotification,omitempty"`
	TestNotification           *TestNotification           `json:"testNotification,omitempty"`
}

// SubscriptionNotification is a subscription lifecycle hint. NotificationType
// is only a hint: the authoritative state comes from subscriptionsv2.get.
type SubscriptionNotification struct {
	Version          string `json:"version"`
	NotificationType int    `json:"notificationType"`
	PurchaseToken    string `json:"purchaseToken"`
	SubscriptionID   string `json:"subscriptionId"`
}

// VoidedPurchaseNotification reports a refunded/charged-back purchase.
type VoidedPurchaseNotification struct {
	PurchaseToken string `json:"purchaseToken"`
	OrderID       string `json:"orderId"`
	ProductType   int    `json:"productType"`
	RefundType    int    `json:"refundType"`
}

// TestNotification is sent from the Play Console "send test" button.
type TestNotification struct {
	Version string `json:"version"`
}

// State is a subscriptionsv2 subscriptionState value. It is a different type
// from entitlements.State: it is Google's raw state, mapped by the Parser.
type State string

// Subscription states returned by purchases.subscriptionsv2.get
// (https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2).
const (
	// StatePending: created, awaiting first payment. Grants nothing.
	StatePending State = "SUBSCRIPTION_STATE_PENDING"
	// StateActive: active; at least one item is auto-renewing and not expired.
	StateActive State = "SUBSCRIPTION_STATE_ACTIVE"
	// StatePaused: paused by the user; no access.
	StatePaused State = "SUBSCRIPTION_STATE_PAUSED"
	// StateInGracePeriod: payment failed, Play still grants access. expiryTime
	// is extended to the end of the grace period.
	StateInGracePeriod State = "SUBSCRIPTION_STATE_IN_GRACE_PERIOD"
	// StateOnHold: account hold after grace; block access.
	StateOnHold State = "SUBSCRIPTION_STATE_ON_HOLD"
	// StateCanceled: auto-renew off, access continues until expiryTime.
	StateCanceled State = "SUBSCRIPTION_STATE_CANCELED"
	// StateExpired: expired; no access.
	StateExpired State = "SUBSCRIPTION_STATE_EXPIRED"
	// StatePendingCanceled: the pending purchase was canceled; the user keeps
	// the previous subscription.
	StatePendingCanceled State = "SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED"
)

// SubscriptionPurchaseV2 is a trimmed subscriptionsv2.get response.
type SubscriptionPurchaseV2 struct {
	SubscriptionState          State                       `json:"subscriptionState"`
	LinkedPurchaseToken        string                      `json:"linkedPurchaseToken,omitempty"`
	LineItems                  []LineItem                  `json:"lineItems,omitempty"`
	ExternalAccountIdentifiers *ExternalAccountIdentifiers `json:"externalAccountIdentifiers,omitempty"`
	AcknowledgementState       string                      `json:"acknowledgementState,omitempty"`
	// TestPurchase is set when Google reports the purchase as a license-tester
	// purchase (the API sends an empty "testPurchase" object).
	TestPurchase bool `json:"-"`
}

// UnmarshalJSON maps the presence of "testPurchase" to TestPurchase.
func (s *SubscriptionPurchaseV2) UnmarshalJSON(b []byte) error {
	type plain SubscriptionPurchaseV2
	aux := struct {
		*plain
		TestPurchase json.RawMessage `json:"testPurchase"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	s.TestPurchase = len(aux.TestPurchase) > 0 && string(aux.TestPurchase) != "null"
	return nil
}

// LineItem is one product in a subscription purchase.
type LineItem struct {
	ProductID string `json:"productId"`
	// ExpiryTime is zero when Google omits it (e.g. a deferred replacement item).
	ExpiryTime time.Time `json:"expiryTime"`
	// LatestSuccessfulOrderID is the order of the most recent successful
	// charge of this item. A voidedPurchaseNotification for any other order
	// does not end the chain.
	LatestSuccessfulOrderID string `json:"latestSuccessfulOrderId,omitempty"`
}

// ExternalAccountIdentifiers carries the developer-supplied account id.
type ExternalAccountIdentifiers struct {
	ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId,omitempty"`
	ObfuscatedExternalProfileID string `json:"obfuscatedExternalProfileId,omitempty"`
}
