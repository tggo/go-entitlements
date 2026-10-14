package google_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/google"
	mocks "github.com/tggo/go-entitlements/internal/mocks/google"
)

const ackPending = "ACKNOWLEDGEMENT_STATE_PENDING"

func parseResult(t *testing.T, sub *google.SubscriptionPurchaseV2) (google.Result, error) {
	t.Helper()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(sub, nil)
	return newParser(t, f, atClock()).ParseResult(context.Background(), subBody(t))
}

func TestParseResult_AckOnlyWhenPendingAndGranting(t *testing.T) {
	t.Parallel()
	exp := clockNow.Add(24 * time.Hour)
	cases := []struct {
		name  string
		state google.State
		ack   string
		want  bool
	}{
		{"active pending", google.StateActive, ackPending, true},
		{"canceled pending", google.StateCanceled, ackPending, true},
		{"grace pending", google.StateInGracePeriod, ackPending, true},
		{"active acknowledged", google.StateActive, "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED", false},
		{"active unspecified", google.StateActive, "", false},
		{"expired pending", google.StateExpired, ackPending, false},
		{"on hold pending", google.StateOnHold, ackPending, false},
		{"paused pending", google.StatePaused, ackPending, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := parseResult(t, &google.SubscriptionPurchaseV2{
				SubscriptionState: tc.state, AcknowledgementState: tc.ack,
				LineItems: []google.LineItem{{ProductID: "premium.yearly", ExpiryTime: exp}},
			})
			require.NoError(t, err)
			if !tc.want {
				assert.Nil(t, res.Ack)
				return
			}
			require.NotNil(t, res.Ack)
			assert.Equal(t, google.Ack{PackageName: pkg, SubscriptionID: "premium.monthly", PurchaseToken: "tok-1"}, *res.Ack)
		})
	}
}

func TestParseResult_AckSubscriptionIDFallsBackToLineItem(t *testing.T) {
	t.Parallel()
	payload := `{"packageName":"com.example.app","eventTimeMillis":"1760436000000","subscriptionNotification":{"notificationType":4,"purchaseToken":"tok-1"}}`
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(&google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateActive, AcknowledgementState: ackPending,
		LineItems: []google.LineItem{{ProductID: "from.item", ExpiryTime: clockNow.Add(time.Hour)}},
	}, nil)

	res, err := newParser(t, f, atClock()).ParseResult(context.Background(), pushOf(t, payload))

	require.NoError(t, err)
	require.NotNil(t, res.Ack)
	assert.Equal(t, "from.item", res.Ack.SubscriptionID)
}

func TestParseResult_TestPurchaseIsAcknowledgedToo(t *testing.T) {
	t.Parallel()
	sub := priorSub(t, "sub_active_obfuscated_account_only.json")
	sub.AcknowledgementState = ackPending

	res, err := parseResult(t, sub)

	require.NoError(t, err)
	assert.Equal(t, entitlements.EnvironmentGoogleTest, res.Events[0].Environment)
	assert.NotNil(t, res.Ack)
}

func TestParseResult_VoidedAndTestNotificationsNeverAck(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "T").Return(&google.SubscriptionPurchaseV2{SubscriptionState: google.StateExpired, AcknowledgementState: ackPending}, nil)
	res, err := newParser(t, f, atClock()).ParseResult(context.Background(), voidedBody(t, "T", "o", clockNow.UnixMilli()))
	require.NoError(t, err)
	assert.Nil(t, res.Ack)
}

func TestParse_StillReturnsTheEvent(t *testing.T) {
	t.Parallel()
	f := mocks.NewMockSubscriptionsFetcher(t)
	f.EXPECT().GetV2(mock.Anything, pkg, "tok-1").Return(&google.SubscriptionPurchaseV2{
		SubscriptionState: google.StateActive, AcknowledgementState: ackPending,
		LineItems: []google.LineItem{{ProductID: "p", ExpiryTime: clockNow.Add(time.Hour)}},
	}, nil)
	ev, err := newParser(t, f, atClock()).Parse(context.Background(), subBody(t))
	require.NoError(t, err)
	assert.Equal(t, entitlements.StateActive, ev.State)
}
