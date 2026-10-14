package google

import "context"

// SubscriptionsFetcher reads the authoritative subscription state from
// purchases.subscriptionsv2.get.
type SubscriptionsFetcher interface {
	GetV2(ctx context.Context, packageName, purchaseToken string) (*SubscriptionPurchaseV2, error)
}
