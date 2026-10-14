package google

import "context"

// SubscriptionsFetcher reads the authoritative subscription state from
// purchases.subscriptionsv2.get.
type SubscriptionsFetcher interface {
	GetV2(ctx context.Context, packageName, purchaseToken string) (*SubscriptionPurchaseV2, error)
}

// Acknowledger acknowledges a subscription purchase
// (purchases.subscriptions.acknowledge). Google refunds purchases that are not
// acknowledged within 3 days. Implementations should classify failures like a
// SubscriptionsFetcher: ErrAlreadyAcknowledged, ErrPurchaseNotFound,
// ErrPurchaseInvalid (permanent), ErrFetchConfig, ErrFetchTransient.
type Acknowledger interface {
	Acknowledge(ctx context.Context, packageName, subscriptionID, purchaseToken string) error
}
