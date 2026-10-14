// Package apple verifies and decodes App Store Server Notifications V2 and
// maps them to entitlements.Event.
package apple

import "encoding/json"

// ResponseBodyV2 is the HTTP body Apple POSTs to the notification endpoint.
type ResponseBodyV2 struct {
	SignedPayload string `json:"signedPayload"`
}

// NotificationData is the "data" object of a decoded notification.
type NotificationData struct {
	AppAppleID            int64  `json:"appAppleId,omitempty"`
	BundleID              string `json:"bundleId,omitempty"`
	BundleVersion         string `json:"bundleVersion,omitempty"`
	Environment           string `json:"environment,omitempty"`
	SignedTransactionInfo string `json:"signedTransactionInfo,omitempty"`
	SignedRenewalInfo     string `json:"signedRenewalInfo,omitempty"`
	// Status: 1 active, 2 expired, 3 billing retry, 4 grace period, 5 revoked.
	Status int `json:"status,omitempty"`
}

// NotificationPayload is the decoded signedPayload (responseBodyV2DecodedPayload).
type NotificationPayload struct {
	NotificationType string            `json:"notificationType"`
	Subtype          string            `json:"subtype,omitempty"`
	NotificationUUID string            `json:"notificationUUID"`
	Version          string            `json:"version,omitempty"`
	SignedDate       int64             `json:"signedDate"` // ms since epoch
	Data             *NotificationData `json:"data,omitempty"`
	// Summary, ExternalPurchaseToken and AppData replace Data on the
	// notification types that have no subscription payload.
	Summary               json.RawMessage `json:"summary,omitempty"`
	ExternalPurchaseToken json.RawMessage `json:"externalPurchaseToken,omitempty"`
	AppData               json.RawMessage `json:"appData,omitempty"`
}

// JWSTransactionDecodedPayload is the decoded signedTransactionInfo.
// All dates are milliseconds since epoch.
type JWSTransactionDecodedPayload struct {
	TransactionID               string `json:"transactionId"`
	OriginalTransactionID       string `json:"originalTransactionId"`
	WebOrderLineItemID          string `json:"webOrderLineItemId,omitempty"`
	BundleID                    string `json:"bundleId,omitempty"`
	ProductID                   string `json:"productId"`
	SubscriptionGroupIdentifier string `json:"subscriptionGroupIdentifier,omitempty"`
	PurchaseDate                int64  `json:"purchaseDate,omitempty"`
	OriginalPurchaseDate        int64  `json:"originalPurchaseDate,omitempty"`
	ExpiresDate                 int64  `json:"expiresDate,omitempty"`
	Quantity                    int    `json:"quantity,omitempty"`
	Type                        string `json:"type,omitempty"`
	AppAccountToken             string `json:"appAccountToken,omitempty"`
	InAppOwnershipType          string `json:"inAppOwnershipType,omitempty"`
	SignedDate                  int64  `json:"signedDate,omitempty"`
	OfferType                   int    `json:"offerType,omitempty"`
	OfferIdentifier             string `json:"offerIdentifier,omitempty"`
	OfferDiscountType           string `json:"offerDiscountType,omitempty"`
	RevocationDate              int64  `json:"revocationDate,omitempty"`
	RevocationReason            int    `json:"revocationReason,omitempty"`
	IsUpgraded                  bool   `json:"isUpgraded,omitempty"`
	Environment                 string `json:"environment,omitempty"`
	TransactionReason           string `json:"transactionReason,omitempty"`
	Storefront                  string `json:"storefront,omitempty"`
	StorefrontID                string `json:"storefrontId,omitempty"`
	Price                       int64  `json:"price,omitempty"`
	Currency                    string `json:"currency,omitempty"`
	AppTransactionID            string `json:"appTransactionId,omitempty"`
}

// JWSRenewalInfoDecodedPayload is the decoded signedRenewalInfo.
type JWSRenewalInfoDecodedPayload struct {
	OriginalTransactionID       string `json:"originalTransactionId,omitempty"`
	AutoRenewProductID          string `json:"autoRenewProductId,omitempty"`
	ProductID                   string `json:"productId,omitempty"`
	AutoRenewStatus             *int   `json:"autoRenewStatus,omitempty"` // 0 off, 1 on; nil when absent
	ExpirationIntent            int    `json:"expirationIntent,omitempty"`
	GracePeriodExpiresDate      int64  `json:"gracePeriodExpiresDate,omitempty"`
	IsInBillingRetryPeriod      bool   `json:"isInBillingRetryPeriod,omitempty"`
	OfferIdentifier             string `json:"offerIdentifier,omitempty"`
	OfferType                   int    `json:"offerType,omitempty"`
	PriceIncreaseStatus         int    `json:"priceIncreaseStatus,omitempty"`
	RecentSubscriptionStartDate int64  `json:"recentSubscriptionStartDate,omitempty"`
	RenewalDate                 int64  `json:"renewalDate,omitempty"`
	RenewalPrice                int64  `json:"renewalPrice,omitempty"`
	Currency                    string `json:"currency,omitempty"`
	SignedDate                  int64  `json:"signedDate,omitempty"`
	Environment                 string `json:"environment,omitempty"`
}

// Notification is a fully verified and decoded notification: the outer
// payload plus its nested transaction and renewal info (nil if absent).
type Notification struct {
	Payload     NotificationPayload
	Transaction *JWSTransactionDecodedPayload
	Renewal     *JWSRenewalInfoDecodedPayload
}
