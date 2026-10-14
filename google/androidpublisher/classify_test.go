package androidpublisher_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	play "google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/androidpublisher"
)

func TestFetcher_ErrorClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want error
		not  []error
	}{
		{http.StatusNotFound, google.ErrPurchaseNotFound, []error{google.ErrFetchConfig, google.ErrFetchTransient}},
		{http.StatusGone, google.ErrPurchaseNotFound, nil},
		{http.StatusBadRequest, google.ErrPurchaseInvalid, []error{google.ErrFetchTransient, google.ErrPurchaseNotFound}},
		{http.StatusUnauthorized, google.ErrFetchConfig, []error{google.ErrPurchaseNotFound, google.ErrFetchTransient}},
		{http.StatusForbidden, google.ErrFetchConfig, nil},
		{http.StatusInternalServerError, google.ErrFetchTransient, []error{google.ErrPurchaseNotFound, google.ErrFetchConfig}},
		{http.StatusServiceUnavailable, google.ErrFetchTransient, nil},
		{http.StatusTooManyRequests, google.ErrFetchTransient, nil},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			t.Parallel()
			f := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, fmt.Sprintf(`{"error":{"code":%d,"message":"x"}}`, tc.code), tc.code)
			})

			_, err := f.GetV2(context.Background(), "p", "t")

			require.ErrorIs(t, err, tc.want)
			for _, n := range tc.not {
				assert.NotErrorIs(t, err, n)
			}
		})
	}
}

func TestFetcher_TransportErrorIsTransient(t *testing.T) {
	t.Parallel()
	f := newFetcher(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.GetV2(ctx, "p", "t")

	assert.ErrorIs(t, err, google.ErrFetchTransient)
}

// The token is part of the request URL; transport errors embed the URL.
func TestFetcher_TransportErrorHasNoToken(t *testing.T) {
	t.Parallel()
	const token = "SECRETPURCHASETOKEN123"
	svc, err := play.NewService(context.Background(), option.WithEndpoint("http://127.0.0.1:1"), option.WithoutAuthentication())
	require.NoError(t, err)

	_, err = androidpublisher.NewFetcher(svc).GetV2(context.Background(), "com.example.app", token)

	require.ErrorIs(t, err, google.ErrFetchTransient)
	assert.NotContains(t, err.Error(), token)
}
