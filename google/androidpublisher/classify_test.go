package androidpublisher_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements/google"
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
