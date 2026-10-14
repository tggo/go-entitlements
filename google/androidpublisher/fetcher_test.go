package androidpublisher_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	play "google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/androidpublisher"
)

func newFetcher(t *testing.T, h http.HandlerFunc) *androidpublisher.Fetcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	svc, err := play.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	return androidpublisher.NewFetcher(svc)
}

func TestAndroidPublisherFetcher_GetV2(t *testing.T) {
	t.Parallel()
	var path string
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE","linkedPurchaseToken":"old",
"acknowledgementState":"ACKNOWLEDGEMENT_STATE_PENDING",
"testPurchase":{},"externalAccountIdentifiers":{"obfuscatedExternalAccountId":"u1","obfuscatedExternalProfileId":"pr1"},
"lineItems":[{"productId":"p1","expiryTime":"2026-11-14T10:00:00Z"},{"productId":"noexp"}]}`))
	})

	got, err := f.GetV2(context.Background(), "com.example.app", "tok")

	require.NoError(t, err)
	assert.Contains(t, path, "com.example.app")
	assert.Contains(t, path, "tok")
	assert.Equal(t, google.StateActive, got.SubscriptionState)
	assert.Equal(t, "old", got.LinkedPurchaseToken)
	assert.Equal(t, "u1", got.ExternalAccountIdentifiers.ObfuscatedExternalAccountID)
	assert.Equal(t, "pr1", got.ExternalAccountIdentifiers.ObfuscatedExternalProfileID)
	assert.True(t, got.TestPurchase)
	require.Len(t, got.LineItems, 2)
	assert.Equal(t, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), got.LineItems[0].ExpiryTime)
	assert.True(t, got.LineItems[1].ExpiryTime.IsZero())
}

func TestAndroidPublisherFetcher_APIError(t *testing.T) {
	t.Parallel()
	f := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":410,"message":"gone"}}`, http.StatusGone)
	})

	_, err := f.GetV2(context.Background(), "p", "t")

	require.ErrorIs(t, err, google.ErrPurchaseNotFound)
}

func TestAndroidPublisherFetcher_BadExpiry(t *testing.T) {
	t.Parallel()
	f := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE","lineItems":[{"expiryTime":"yesterday"}]}`))
	})

	_, err := f.GetV2(context.Background(), "p", "t")

	require.ErrorIs(t, err, androidpublisher.ErrBadResponse)
	assert.NotErrorIs(t, err, google.ErrFetchTransient, "a bad body is not a transport failure")
}

func TestAndroidPublisherFetcher_AutoRenewingPlan(t *testing.T) {
	t.Parallel()
	f := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"subscriptionState":"SUBSCRIPTION_STATE_CANCELED","lineItems":[
{"productId":"a","expiryTime":"2026-11-14T10:00:00Z","autoRenewingPlan":{"autoRenewEnabled":false}},
{"productId":"b","expiryTime":"2026-11-14T10:00:00Z","autoRenewingPlan":{"autoRenewEnabled":true}},
{"productId":"c","expiryTime":"2026-11-14T10:00:00Z","prepaidPlan":{}}]}`))
	})

	got, err := f.GetV2(context.Background(), "p", "t")

	require.NoError(t, err)
	require.Len(t, got.LineItems, 3)
	require.NotNil(t, got.LineItems[0].AutoRenewingPlan)
	assert.False(t, got.LineItems[0].AutoRenewingPlan.AutoRenewEnabled, "an explicit false is kept, not confused with absent")
	require.NotNil(t, got.LineItems[1].AutoRenewingPlan)
	assert.True(t, got.LineItems[1].AutoRenewingPlan.AutoRenewEnabled)
	assert.Nil(t, got.LineItems[2].AutoRenewingPlan, "a prepaid plan has no auto-renew setting")
}
