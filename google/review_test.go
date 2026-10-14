package google_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/google"
	"github.com/tggo/go-entitlements/internal/testutil"
)

func newParser(tb testing.TB, f google.SubscriptionsFetcher, opts ...google.Option) *google.Parser {
	tb.Helper()
	return newParserFor(tb, f, pkg, opts...)
}

func newParserFor(tb testing.TB, f google.SubscriptionsFetcher, packageName string, opts ...google.Option) *google.Parser {
	tb.Helper()
	p, err := google.NewParser(f, packageName, opts...)
	require.NoError(tb, err)
	return p
}

func subBody(tb testing.TB) []byte {
	tb.Helper()
	return testutil.GooglePushBody(tb, "m", testutil.GoogleFixture(tb, "notif_subscription.json"))
}

func TestNewParser_NilFetcher(t *testing.T) {
	t.Parallel()
	p, err := google.NewParser(nil, pkg)
	require.Error(t, err)
	assert.Nil(t, p)
}

func TestNewParser_NilOptionIgnored(t *testing.T) {
	t.Parallel()
	p, err := google.NewParser(mocks.NewMockSubscriptionsFetcher(t), pkg, nil, nil)
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestParse_RawIsACopy(t *testing.T) {
	t.Parallel()
	body := testutil.GooglePushBody(t, "msg-v", testutil.GoogleFixture(t, "notif_voided.json"))
	orig := append([]byte(nil), body...)
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired}, nil)

	ev, err := newParser(t, f).Parse(context.Background(), body)
	require.NoError(t, err)
	for i := range body {
		body[i] = 'x'
	}

	assert.Equal(t, orig, ev.Raw)
}

func TestParse_VoidedWithoutTokenIsMalformed(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"packageName":"com.example.app","eventTimeMillis":"1760436000000","voidedPurchaseNotification":{"orderId":"o","productType":1,"refundType":1}}`)
	_, err := newParser(t, mocks.NewMockSubscriptionsFetcher(t)).Parse(context.Background(), testutil.GooglePushBody(t, "m", payload))
	assert.ErrorIs(t, err, google.ErrMalformed)
}

func TestParse_FetchErrorClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"already not found", fmt.Errorf("x: %w", google.ErrPurchaseNotFound), google.ErrPurchaseNotFound},
		{"already config", fmt.Errorf("x: %w", google.ErrFetchConfig), google.ErrFetchConfig},
		{"already transient", fmt.Errorf("x: %w", google.ErrFetchTransient), google.ErrFetchTransient},
		{"deadline", fmt.Errorf("do: %w", context.DeadlineExceeded), google.ErrFetchTransient},
		{"plain", errors.New("connection reset"), google.ErrFetchTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := mocks.NewMockSubscriptionsFetcher(t)
			f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(nil, tc.err)

			_, err := newParser(t, f).Parse(context.Background(), subBody(t))

			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, tc.err, "original error stays reachable")
			for _, other := range []error{google.ErrPurchaseNotFound, google.ErrFetchConfig, google.ErrFetchTransient} {
				if !errors.Is(tc.want, other) {
					assert.NotErrorIs(t, err, other)
				}
			}
		})
	}
}

func TestParse_FetchTimeout(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").
		RunAndReturn(func(ctx context.Context, _, _ string) (*google.SubscriptionPurchaseV2, error) {
			dl, ok := ctx.Deadline()
			assert.True(t, ok, "fetch context must carry a deadline")
			assert.WithinDuration(t, time.Now().Add(50*time.Millisecond), dl, time.Second)
			<-ctx.Done()
			return nil, ctx.Err()
		})

	_, err := newParser(t, f, google.WithFetchTimeout(50*time.Millisecond)).Parse(context.Background(), subBody(t))

	require.ErrorIs(t, err, google.ErrFetchTransient)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestParser_ConcurrentUse(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(priorSub(t, "sub_active_profile_id_only.json"), nil)
	p := newParser(t, f)
	body := subBody(t)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			ev, err := p.Parse(context.Background(), body)
			assert.NoError(t, err)
			assert.Equal(t, entitlements.StateActive, ev.State)
		})
	}
	wg.Wait()
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
