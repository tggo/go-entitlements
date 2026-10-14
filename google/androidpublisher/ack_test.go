package androidpublisher_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	play "google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/tggo/go-entitlements/google"
	"github.com/tggo/go-entitlements/google/androidpublisher"
)

func TestAcknowledge_Request(t *testing.T) {
	t.Parallel()
	var method, path string
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	})

	err := f.Acknowledge(context.Background(), "com.example.app", "premium.monthly", "tok-1")

	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, method)
	assert.Contains(t, path, "/applications/com.example.app/purchases/subscriptions/premium.monthly/tokens/tok-1:acknowledge")
}

func TestAcknowledge_ErrorClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want error
	}{
		{http.StatusNotFound, google.ErrPurchaseNotFound},
		{http.StatusGone, google.ErrPurchaseNotFound},
		{http.StatusBadRequest, google.ErrPurchaseInvalid},
		{http.StatusConflict, google.ErrAlreadyAcknowledged},
		{http.StatusUnauthorized, google.ErrFetchConfig},
		{http.StatusForbidden, google.ErrFetchConfig},
		{http.StatusTooManyRequests, google.ErrFetchTransient},
		{http.StatusInternalServerError, google.ErrFetchTransient},
		{http.StatusServiceUnavailable, google.ErrFetchTransient},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			t.Parallel()
			f := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, fmt.Sprintf(`{"error":{"code":%d,"message":"x"}}`, tc.code), tc.code)
			})

			err := f.Acknowledge(context.Background(), "p", "s", "t")

			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestAcknowledge_TransportErrorHasNoToken(t *testing.T) {
	t.Parallel()
	const token = "SECRETPURCHASETOKEN123"
	svc, err := play.NewService(context.Background(), option.WithEndpoint("http://127.0.0.1:1"), option.WithoutAuthentication())
	require.NoError(t, err)

	err = androidpublisher.NewFetcher(svc).Acknowledge(context.Background(), "com.example.app", "s", token)

	require.ErrorIs(t, err, google.ErrFetchTransient)
	assert.NotContains(t, err.Error(), token)
}
