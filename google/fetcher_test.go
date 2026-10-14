package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/tggo/go-entitlements/google"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func newFetcher(t *testing.T, h http.HandlerFunc) *google.AndroidPublisherFetcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	svc, err := androidpublisher.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	return google.NewAndroidPublisherFetcher(svc)
}

func TestAndroidPublisherFetcher_GetV2(t *testing.T) {
	var path string
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE","linkedPurchaseToken":"old",
"acknowledgementState":"ACKNOWLEDGEMENT_STATE_PENDING",
"externalAccountIdentifiers":{"obfuscatedExternalAccountId":"u1"},
"lineItems":[{"productId":"p1","expiryTime":"2026-11-14T10:00:00Z"},{"productId":"noexp"}]}`))
	})

	got, err := f.GetV2(context.Background(), "com.example.app", "tok")

	require.NoError(t, err)
	assert.Contains(t, path, "com.example.app")
	assert.Contains(t, path, "tok")
	assert.Equal(t, google.StateActive, got.SubscriptionState)
	assert.Equal(t, "old", got.LinkedPurchaseToken)
	assert.Equal(t, "u1", got.ExternalAccountIdentifiers.ObfuscatedExternalAccountID)
	require.Len(t, got.LineItems, 2)
	assert.Equal(t, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), got.LineItems[0].ExpiryTime)
	assert.True(t, got.LineItems[1].ExpiryTime.IsZero())
}

func TestAndroidPublisherFetcher_APIError(t *testing.T) {
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":410,"message":"gone"}}`, http.StatusGone)
	})

	_, err := f.GetV2(context.Background(), "p", "t")

	assert.Error(t, err)
}

func TestAndroidPublisherFetcher_BadExpiry(t *testing.T) {
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE","lineItems":[{"expiryTime":"yesterday"}]}`))
	})

	_, err := f.GetV2(context.Background(), "p", "t")

	assert.Error(t, err)
}
