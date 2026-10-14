package serverapi

import (
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// Status is the state of an auto-renewable subscription as the App Store
// reports it (https://developer.apple.com/documentation/appstoreserverapi/status).
type Status int

// Subscription statuses.
const (
	StatusActive       Status = 1
	StatusExpired      Status = 2
	StatusBillingRetry Status = 3
	StatusGracePeriod  Status = 4
	StatusRevoked      Status = 5
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusExpired:
		return "expired"
	case StatusBillingRetry:
		return "billing_retry"
	case StatusGracePeriod:
		return "grace_period"
	case StatusRevoked:
		return "revoked"
	}
	return "unknown"
}

// SubscriptionStatuses is the verified answer of Get All Subscription Statuses.
type SubscriptionStatuses struct {
	Environment entitlements.Environment
	BundleID    string
	AppAppleID  int64
	Groups      []SubscriptionGroup
}

// SubscriptionGroup holds the latest transaction of each subscription chain
// in one subscription group.
type SubscriptionGroup struct {
	SubscriptionGroupIdentifier string
	Subscriptions               []SubscriptionStatus
}

// SubscriptionStatus is one verified lastTransactions item.
type SubscriptionStatus struct {
	OriginalTransactionID string
	Status                Status
	// Transaction is the verified signedTransactionInfo.
	Transaction apple.JWSTransactionDecodedPayload
	// Renewal is the verified signedRenewalInfo (nil if Apple sent none).
	Renewal *apple.JWSRenewalInfoDecodedPayload
	// SignedTransactionInfo and SignedRenewalInfo are the JWS strings the
	// values above were verified from.
	SignedTransactionInfo string
	SignedRenewalInfo     string
}

// HistoryQuery filters Get Transaction History. The zero value asks for the
// whole history, oldest first. Filters are ANDed; several values of one
// filter are ORed.
type HistoryQuery struct {
	// StartDate (inclusive) and EndDate (exclusive) bound the purchase date.
	StartDate, EndDate time.Time
	ProductIDs         []string
	// ProductTypes: AUTO_RENEWABLE, NON_RENEWABLE, CONSUMABLE, NON_CONSUMABLE.
	ProductTypes []string
	// InAppOwnershipType: FAMILY_SHARED or PURCHASED.
	InAppOwnershipType string
	// Sort: ASCENDING (default) or DESCENDING, by last modification.
	Sort string
	// Revoked, when set, keeps only revoked (true) or only unrevoked (false)
	// transactions.
	Revoked              *bool
	SubscriptionGroupIDs []string
}

// HistoryPage is one verified page of up to 20 transactions.
type HistoryPage struct {
	Environment entitlements.Environment
	BundleID    string
	AppAppleID  int64
	// HasMore says another page exists; pass Revision to get it.
	HasMore      bool
	Revision     string
	Transactions []apple.JWSTransactionDecodedPayload
}

// OrderLookup is the verified answer of Look Up Order ID.
type OrderLookup struct {
	// Valid is true when the order ID belongs to at least one in-app
	// purchase of this app (OrderLookupStatus 0).
	Valid        bool
	Transactions []apple.JWSTransactionDecodedPayload
}

// SendAttempt is one attempt of the App Store to deliver a notification.
type SendAttempt struct {
	AttemptDate time.Time
	// Result is Apple's sendAttemptResult, e.g. SUCCESS, TIMED_OUT,
	// NO_RESPONSE, UNSUCCESSFUL_HTTP_RESPONSE_CODE, OTHER.
	Result string
}

// TestNotificationStatus is the answer of Get Test Notification Status.
type TestNotificationStatus struct {
	FirstSendAttemptResult string
	SendAttempts           []SendAttempt
	// SignedPayload is the TEST notification JWS as Apple returned it.
	SignedPayload string
	// Notification is SignedPayload verified and decoded; nil when
	// verification failed (see VerifyErr).
	Notification *apple.Notification
	VerifyErr    error
}

// NotificationHistoryRequest selects notifications for Get Notification
// History. Start and End are required; give either TransactionID or a
// NotificationType (Apple rejects both together).
type NotificationHistoryRequest struct {
	Start, End time.Time
	// NotificationType and NotificationSubtype filter by type; a subtype
	// needs its type.
	NotificationType    string
	NotificationSubtype string
	// OnlyFailures keeps only notifications that did not reach your server
	// (including ones still being retried).
	OnlyFailures bool
	// TransactionID limits the history to one customer.
	TransactionID string
}

// NotificationRecord is one notification the App Store tried to send.
type NotificationRecord struct {
	// SignedPayload is the JWS exactly as Apple returned it. It is not
	// verified: feed it to apple.Parser, or use Notification.
	SignedPayload string
	// Notification is SignedPayload verified and decoded with the Client's
	// Verifier; nil when verification failed (see VerifyErr), for example
	// because the signing certificate has expired since the notification was
	// sent.
	Notification           *apple.Notification
	VerifyErr              error
	FirstSendAttemptResult string
	SendAttempts           []SendAttempt
}

// NotificationHistoryPage is one page of up to 20 records.
type NotificationHistoryPage struct {
	Records []NotificationRecord
	HasMore bool
	// PaginationToken fetches the next page when HasMore is true.
	PaginationToken string
}
