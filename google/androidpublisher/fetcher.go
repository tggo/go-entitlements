// Package androidpublisher implements google.SubscriptionsFetcher over the
// Google Play Developer API (androidpublisher v3). It lives in its own package
// so that users of package google do not pull in the Google API client.
package androidpublisher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	play "google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/googleapi"

	"github.com/tggo/go-entitlements/google"
)

// Fetcher implements google.SubscriptionsFetcher. It is safe for concurrent
// use. Timeouts and the HTTP client come from the play.Service you pass in
// (and from google.WithFetchTimeout on the parser).
type Fetcher struct {
	svc *play.Service
}

// NewFetcher wraps an already-configured androidpublisher service.
func NewFetcher(svc *play.Service) *Fetcher {
	return &Fetcher{svc: svc}
}

// GetV2 implements google.SubscriptionsFetcher. Failures are wrapped with one
// of google.ErrPurchaseNotFound (404, 410), google.ErrFetchConfig (401, 403)
// or google.ErrFetchTransient (everything else); the original error stays
// reachable through errors.Is/As.
func (f *Fetcher) GetV2(ctx context.Context, packageName, purchaseToken string) (*google.SubscriptionPurchaseV2, error) {
	resp, err := f.svc.Purchases.Subscriptionsv2.Get(packageName, purchaseToken).Context(ctx).Do()
	if err != nil {
		return nil, classify(err)
	}
	return convertV2(resp)
}

func classify(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusNotFound, http.StatusGone:
			return fmt.Errorf("androidpublisher: subscriptionsv2.get: %w: %w", google.ErrPurchaseNotFound, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("androidpublisher: subscriptionsv2.get: %w: %w", google.ErrFetchConfig, err)
		}
	}
	return fmt.Errorf("androidpublisher: subscriptionsv2.get: %w: %w", google.ErrFetchTransient, err)
}

func convertV2(resp *play.SubscriptionPurchaseV2) (*google.SubscriptionPurchaseV2, error) {
	out := &google.SubscriptionPurchaseV2{
		SubscriptionState:    resp.SubscriptionState,
		LinkedPurchaseToken:  resp.LinkedPurchaseToken,
		AcknowledgementState: resp.AcknowledgementState,
		TestPurchase:         resp.TestPurchase != nil,
	}
	if ids := resp.ExternalAccountIdentifiers; ids != nil {
		out.ExternalAccountIdentifiers = &google.ExternalAccountIdentifiers{
			ObfuscatedExternalAccountID: ids.ObfuscatedExternalAccountId,
			ObfuscatedExternalProfileID: ids.ObfuscatedExternalProfileId,
		}
	}
	for _, li := range resp.LineItems {
		if li == nil {
			continue
		}
		item := google.LineItem{ProductID: li.ProductId}
		if li.ExpiryTime != "" {
			t, err := time.Parse(time.RFC3339Nano, li.ExpiryTime)
			if err != nil {
				return nil, fmt.Errorf("androidpublisher: bad expiryTime %q: %w", li.ExpiryTime, err)
			}
			item.ExpiryTime = t.UTC()
		}
		out.LineItems = append(out.LineItems, item)
	}
	return out, nil
}
