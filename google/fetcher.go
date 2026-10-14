package google

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/api/androidpublisher/v3"
)

// AndroidPublisherFetcher implements SubscriptionsFetcher over the
// androidpublisher v3 API.
type AndroidPublisherFetcher struct {
	svc *androidpublisher.Service
}

// NewAndroidPublisherFetcher wraps an already-configured androidpublisher service.
func NewAndroidPublisherFetcher(svc *androidpublisher.Service) *AndroidPublisherFetcher {
	return &AndroidPublisherFetcher{svc: svc}
}

// GetV2 implements SubscriptionsFetcher.
func (f *AndroidPublisherFetcher) GetV2(ctx context.Context, packageName, purchaseToken string) (*SubscriptionPurchaseV2, error) {
	resp, err := f.svc.Purchases.Subscriptionsv2.Get(packageName, purchaseToken).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("google: subscriptionsv2.get: %w", err)
	}
	return convertV2(resp)
}

func convertV2(resp *androidpublisher.SubscriptionPurchaseV2) (*SubscriptionPurchaseV2, error) {
	out := &SubscriptionPurchaseV2{
		SubscriptionState:    resp.SubscriptionState,
		LinkedPurchaseToken:  resp.LinkedPurchaseToken,
		AcknowledgementState: resp.AcknowledgementState,
	}
	if resp.ExternalAccountIdentifiers != nil {
		out.ExternalAccountIdentifiers = &ExternalAccountIdentifiers{
			ObfuscatedExternalAccountID: resp.ExternalAccountIdentifiers.ObfuscatedExternalAccountId,
		}
	}
	for _, li := range resp.LineItems {
		if li == nil {
			continue
		}
		item := LineItem{ProductID: li.ProductId}
		if li.ExpiryTime != "" {
			t, err := time.Parse(time.RFC3339Nano, li.ExpiryTime)
			if err != nil {
				return nil, fmt.Errorf("google: bad expiryTime %q: %w", li.ExpiryTime, err)
			}
			item.ExpiryTime = t.UTC()
		}
		out.LineItems = append(out.LineItems, item)
	}
	return out, nil
}
