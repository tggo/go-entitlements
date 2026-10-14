package serverapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/internal/testutil"
)

var ctx = context.Background()

func TestGetAllSubscriptionStatuses(t *testing.T) {
	f := newFakeApple(t)
	active, grace := loadStatus(t, "status_active"), loadStatus(t, "status_grace")
	other := grace.with(map[string]any{"originalTransactionId": "2000000000000099", "transactionId": "2000000000000100"})
	f.respond(ok(statusResponse("Sandbox",
		[]map[string]any{active.wireItem(t, f.ca)},
		[]map[string]any{other.wireItem(t, f.ca)},
	)))
	c, _ := f.client()

	got, err := c.GetAllSubscriptionStatuses(ctx, "2000000000000001", StatusActive, StatusGracePeriod)
	require.NoError(t, err)

	reqs := f.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, http.MethodGet, reqs[0].Method)
	assert.Equal(t, "/inApps/v1/subscriptions/2000000000000001", reqs[0].Path)
	assert.Equal(t, []string{"1", "4"}, reqs[0].Query["status"], "status repeats once per value")

	assert.Equal(t, entitlements.EnvironmentSandbox, got.Environment)
	assert.Equal(t, testBundle, got.BundleID)
	assert.EqualValues(t, 1234567890, got.AppAppleID)
	require.Len(t, got.Groups, 2)
	a := got.Groups[0].Subscriptions[0]
	assert.Equal(t, StatusActive, a.Status)
	assert.Equal(t, origTx, a.OriginalTransactionID)
	assert.Equal(t, "com.example.app.monthly", a.Transaction.ProductID)
	require.NotNil(t, a.Renewal)
	require.NotNil(t, a.Renewal.AutoRenewStatus)
	assert.Equal(t, 1, *a.Renewal.AutoRenewStatus)
	assert.NotEmpty(t, a.SignedTransactionInfo)
	g := got.Groups[1].Subscriptions[0]
	assert.Equal(t, StatusGracePeriod, g.Status)
	assert.EqualValues(t, 1791849600000, g.Renewal.GracePeriodExpiresDate) // 2026-10-13
}

func TestGetAllSubscriptionStatuses_NoStatusFilterSendsNoQuery(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(statusResponse("Sandbox")))
	c, _ := f.client()
	got, err := c.GetAllSubscriptionStatuses(ctx, "1")
	require.NoError(t, err)
	assert.Empty(t, got.Groups)
	assert.Empty(t, f.requests()[0].Query)
}

func TestEndpoints_EmptyIDIsRejectedBeforeAnyRequest(t *testing.T) {
	f := newFakeApple(t)
	c, _ := f.client()
	_, err := c.GetAllSubscriptionStatuses(ctx, "")
	require.ErrorContains(t, err, "transaction ID is required")
	_, err = c.GetTransactionHistory(ctx, "", HistoryQuery{}, "")
	require.Error(t, err)
	_, err = c.GetTransactionInfo(ctx, "")
	require.Error(t, err)
	_, err = c.LookUpOrderID(ctx, "")
	require.Error(t, err)
	_, err = c.GetTestNotificationStatus(ctx, "")
	require.Error(t, err)
	_, err = c.GetNotificationHistory(ctx, NotificationHistoryRequest{}, "")
	require.Error(t, err)
	assert.Empty(t, f.requests())
}

func TestEndpoints_PathParametersAreEscaped(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(404, 4040010, "x"))
	c, _ := f.client()
	_, _ = c.GetTransactionInfo(ctx, "../v1/notifications/test")
	require.Len(t, f.requests(), 1)
	assert.Equal(t, "/inApps/v1/transactions/..%2Fv1%2Fnotifications%2Ftest", f.requests()[0].Path)
}

// Every signed value the API returns is verified and pinned; each case
// below must be refused rather than decoded.
func TestGetAllSubscriptionStatuses_RefusesUntrustedData(t *testing.T) {
	active := loadStatus(t, "status_active")
	rogue := testutil.NewAppleCA(t)

	tests := []struct {
		name string
		body func(f *fakeApple) map[string]any
		is   error
	}{
		{"transaction signed by another CA", func(f *fakeApple) map[string]any {
			return statusResponse("Sandbox", []map[string]any{active.wireItem(t, rogue)})
		}, entitlements.ErrInvalidSignature},
		{"renewal signed by another CA", func(f *fakeApple) map[string]any {
			item := active.wireItem(t, f.ca)
			item["signedRenewalInfo"] = rogue.Sign(t, active.Renewal)
			return statusResponse("Sandbox", []map[string]any{item})
		}, entitlements.ErrInvalidSignature},
		{"transaction of another app", func(f *fakeApple) map[string]any {
			return statusResponse("Sandbox", []map[string]any{active.with(map[string]any{"bundleId": "com.evil.app"}).wireItem(t, f.ca)})
		}, ErrIdentityMismatch},
		{"production transaction in a sandbox client", func(f *fakeApple) map[string]any {
			return statusResponse("Sandbox", []map[string]any{active.with(map[string]any{"environment": "Production"}).wireItem(t, f.ca)})
		}, ErrIdentityMismatch},
		{"transaction without environment", func(f *fakeApple) map[string]any {
			return statusResponse("Sandbox", []map[string]any{active.with(map[string]any{"environment": ""}).wireItem(t, f.ca)})
		}, ErrIdentityMismatch},
		{"renewal of another environment", func(f *fakeApple) map[string]any {
			item := active.wireItem(t, f.ca)
			ri := map[string]any{"originalTransactionId": origTx, "environment": "Production", "signedDate": 1}
			item["signedRenewalInfo"] = f.ca.Sign(t, ri)
			return statusResponse("Sandbox", []map[string]any{item})
		}, ErrIdentityMismatch},
		{"response of another app", func(f *fakeApple) map[string]any {
			r := statusResponse("Sandbox", []map[string]any{active.wireItem(t, f.ca)})
			r["bundleId"] = "com.evil.app"
			return r
		}, ErrIdentityMismatch},
		{"response of another environment", func(f *fakeApple) map[string]any {
			return statusResponse("Production", []map[string]any{active.wireItem(t, f.ca)})
		}, ErrIdentityMismatch},
		{"unsigned garbage", func(f *fakeApple) map[string]any {
			item := active.wireItem(t, f.ca)
			item["signedTransactionInfo"] = "a.b.c"
			return statusResponse("Sandbox", []map[string]any{item})
		}, entitlements.ErrInvalidSignature},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApple(t)
			f.respond(func(w http.ResponseWriter, _ recorded) { writeJSON(w, 200, tc.body(f)) })
			c, _ := f.client()
			got, err := c.GetAllSubscriptionStatuses(ctx, "1")
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.is)
			assert.Nil(t, got, "nothing partially verified is returned")
		})
	}
}

func TestIdentityMismatchIsAnInvalidSignature(t *testing.T) {
	assert.ErrorIs(t, ErrIdentityMismatch, entitlements.ErrInvalidSignature)
}

func TestGetTransactionHistory_QueryParameters(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{
		"appAppleId": 1, "bundleId": testBundle, "environment": "Sandbox", "hasMore": true, "revision": "rev-1",
		"signedTransactions": []string{f.ca.Sign(t, tx("t1", nil)), f.ca.Sign(t, tx("t2", nil))},
	}))
	c, _ := f.client()
	revoked := false
	start, end := time.UnixMilli(1790000000000), time.UnixMilli(1791000000000)

	page, err := c.GetTransactionHistory(ctx, "T0", HistoryQuery{
		StartDate: start, EndDate: end,
		ProductIDs: []string{"p1", "p2"}, ProductTypes: []string{"AUTO_RENEWABLE", "CONSUMABLE"},
		InAppOwnershipType: "PURCHASED", Sort: "DESCENDING", Revoked: &revoked,
		SubscriptionGroupIDs: []string{"g1"},
	}, "rev-0")
	require.NoError(t, err)

	r := f.requests()[0]
	assert.Equal(t, "/inApps/v2/history/T0", r.Path)
	assert.Equal(t, map[string][]string{
		"revision": {"rev-0"}, "startDate": {"1790000000000"}, "endDate": {"1791000000000"},
		"productId": {"p1", "p2"}, "productType": {"AUTO_RENEWABLE", "CONSUMABLE"},
		"inAppOwnershipType": {"PURCHASED"}, "sort": {"DESCENDING"}, "revoked": {"false"},
		"subscriptionGroupIdentifier": {"g1"},
	}, r.Query)
	assert.True(t, page.HasMore)
	assert.Equal(t, "rev-1", page.Revision)
	require.Len(t, page.Transactions, 2)
	assert.Equal(t, "t2", page.Transactions[1].TransactionID)
}

func TestGetTransactionHistory_ZeroQueryIsEmpty(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"bundleId": testBundle, "environment": "Sandbox"}))
	c, _ := f.client()
	page, err := c.GetTransactionHistory(ctx, "T0", HistoryQuery{}, "")
	require.NoError(t, err)
	assert.Empty(t, f.requests()[0].Query)
	assert.Empty(t, page.Transactions)
	assert.False(t, page.HasMore)
}

func TestGetTransactionHistory_RefusesUntrustedTransactions(t *testing.T) {
	f := newFakeApple(t)
	rogue := testutil.NewAppleCA(t)
	f.respond(ok(map[string]any{
		"bundleId": testBundle, "environment": "Sandbox",
		"signedTransactions": []string{f.ca.Sign(t, tx("t1", nil)), rogue.Sign(t, tx("t2", nil))},
	}))
	c, _ := f.client()
	page, err := c.GetTransactionHistory(ctx, "T0", HistoryQuery{}, "")
	require.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	assert.ErrorContains(t, err, "signedTransactions[1]")
	assert.Nil(t, page)
}

// historyServer serves nPages pages of 2 transactions; revision "rN" fetches
// page N.
func historyServer(t *testing.T, f *fakeApple, nPages int) {
	t.Helper()
	f.respond(func(w http.ResponseWriter, r recorded) {
		page := 0
		if rev := r.Query["revision"]; len(rev) > 0 {
			n, err := strconv.Atoi(rev[0][1:])
			require.NoError(t, err)
			page = n
		}
		resp := map[string]any{
			"bundleId": testBundle, "environment": "Sandbox", "hasMore": page < nPages-1, "revision": "r" + strconv.Itoa(page+1),
			"signedTransactions": []string{
				f.ca.Sign(t, tx("p"+strconv.Itoa(page)+"a", nil)), f.ca.Sign(t, tx("p"+strconv.Itoa(page)+"b", nil)),
			},
		}
		writeJSON(w, 200, resp)
	})
}

func TestTransactionHistory_WalksAllPages(t *testing.T) {
	f := newFakeApple(t)
	historyServer(t, f, 3)
	c, _ := f.client()

	var ids []string
	for tx, err := range c.TransactionHistory(ctx, "T0", HistoryQuery{Sort: "ASCENDING"}) {
		require.NoError(t, err)
		ids = append(ids, tx.TransactionID)
	}
	assert.Equal(t, []string{"p0a", "p0b", "p1a", "p1b", "p2a", "p2b"}, ids)

	reqs := f.requests()
	require.Len(t, reqs, 3)
	assert.Empty(t, reqs[0].Query["revision"], "first page has no revision")
	assert.Equal(t, []string{"r1"}, reqs[1].Query["revision"])
	assert.Equal(t, []string{"r2"}, reqs[2].Query["revision"])
	for _, r := range reqs {
		assert.Equal(t, []string{"ASCENDING"}, r.Query["sort"], "the same query is sent with every page")
	}
}

func TestTransactionHistory_StopsWhenTheCallerBreaks(t *testing.T) {
	f := newFakeApple(t)
	historyServer(t, f, 3)
	c, _ := f.client()
	n := 0
	for _, err := range c.TransactionHistory(ctx, "T0", HistoryQuery{}) {
		require.NoError(t, err)
		n++
		if n == 1 {
			break
		}
	}
	assert.Len(t, f.requests(), 1, "no page is fetched after the loop ends")
}

func TestTransactionHistory_ErrorEndsTheWalk(t *testing.T) {
	f := newFakeApple(t)
	f.script(
		ok(map[string]any{"bundleId": testBundle, "environment": "Sandbox", "hasMore": true, "revision": "r1",
			"signedTransactions": []string{f.ca.Sign(t, tx("a", nil))}}),
		apiErr(404, 4040010, "gone"),
	)
	c, _ := f.client()
	var got []string
	var errs []error
	for tx, err := range c.TransactionHistory(ctx, "T0", HistoryQuery{}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		got = append(got, tx.TransactionID)
	}
	assert.Equal(t, []string{"a"}, got)
	require.Len(t, errs, 1)
	assert.ErrorIs(t, errs[0], ErrTransactionNotFound)
}

func TestTransactionHistory_StuckCursorDoesNotLoopForever(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"bundleId": testBundle, "environment": "Sandbox", "hasMore": true, "revision": "same"}))
	c, _ := f.client()
	var last error
	for _, err := range c.TransactionHistory(ctx, "T0", HistoryQuery{}) {
		last = err
	}
	require.ErrorIs(t, last, errPagingStuck)
	assert.LessOrEqual(t, len(f.requests()), 2)

	f2 := newFakeApple(t)
	f2.respond(ok(map[string]any{"bundleId": testBundle, "environment": "Sandbox", "hasMore": true}))
	c2, _ := f2.client()
	last = nil
	for _, err := range c2.TransactionHistory(ctx, "T0", HistoryQuery{}) {
		last = err
	}
	require.ErrorIs(t, last, errPagingStuck, "hasMore without a revision")
}

func TestGetTransactionInfo(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"signedTransactionInfo": f.ca.Sign(t, tx("2000000000000002", map[string]any{"productId": "pro"}))}))
	c, _ := f.client()

	got, err := c.GetTransactionInfo(ctx, "2000000000000002")
	require.NoError(t, err)
	assert.Equal(t, "pro", got.ProductID)
	assert.Equal(t, "/inApps/v1/transactions/2000000000000002", f.requests()[0].Path)
	assert.Equal(t, http.MethodGet, f.requests()[0].Method)
}

func TestGetTransactionInfo_RefusesForeignTransaction(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"signedTransactionInfo": f.ca.Sign(t, tx("1", map[string]any{"bundleId": "com.evil.app"}))}))
	c, _ := f.client()
	_, err := c.GetTransactionInfo(ctx, "1")
	assert.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestLookUpOrderID(t *testing.T) {
	t.Run("valid order", func(t *testing.T) {
		f := newFakeApple(t)
		f.respond(ok(map[string]any{"status": 0, "signedTransactions": []string{f.ca.Sign(t, tx("t1", nil)), f.ca.Sign(t, tx("t2", nil))}}))
		c, _ := f.client()
		got, err := c.LookUpOrderID(ctx, "MPFQ65H8GA")
		require.NoError(t, err)
		assert.True(t, got.Valid)
		assert.Len(t, got.Transactions, 2)
		assert.Equal(t, "/inApps/v1/lookup/MPFQ65H8GA", f.requests()[0].Path)
	})
	t.Run("unknown order is not an error", func(t *testing.T) {
		f := newFakeApple(t)
		f.respond(ok(map[string]any{"status": 1}))
		c, _ := f.client()
		got, err := c.LookUpOrderID(ctx, "NOPE")
		require.NoError(t, err)
		assert.False(t, got.Valid)
		assert.Empty(t, got.Transactions)
	})
	t.Run("untrusted transaction is refused", func(t *testing.T) {
		f := newFakeApple(t)
		f.respond(ok(map[string]any{"status": 0, "signedTransactions": []string{testutil.NewAppleCA(t).Sign(t, tx("t1", nil))}}))
		c, _ := f.client()
		_, err := c.LookUpOrderID(ctx, "X")
		assert.ErrorIs(t, err, entitlements.ErrInvalidSignature)
	})
}

func TestTestNotification_RequestAndStatus(t *testing.T) {
	f := newFakeApple(t)
	signed := notificationSigned(t, f.ca, "TEST", "", "uuid-test", 1791633600000, nil, 0)
	f.respond(func(w http.ResponseWriter, r recorded) {
		switch r.Method + " " + r.Path {
		case "POST /inApps/v1/notifications/test":
			writeJSON(w, 200, map[string]any{"testNotificationToken": "tok-1"})
		case "GET /inApps/v1/notifications/test/tok-1":
			writeJSON(w, 200, map[string]any{
				"signedPayload": signed, "firstSendAttemptResult": "SUCCESS",
				"sendAttempts": []map[string]any{{"attemptDate": 1791633600000, "sendAttemptResult": "SUCCESS"}},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.Path)
			w.WriteHeader(500)
		}
	})
	c, _ := f.client()

	token, err := c.RequestTestNotification(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tok-1", token)

	st, err := c.GetTestNotificationStatus(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "SUCCESS", st.FirstSendAttemptResult)
	require.Len(t, st.SendAttempts, 1)
	assert.Equal(t, "SUCCESS", st.SendAttempts[0].Result)
	assert.Equal(t, time.UnixMilli(1791633600000).UTC(), st.SendAttempts[0].AttemptDate)
	require.NoError(t, st.VerifyErr)
	require.NotNil(t, st.Notification)
	assert.Equal(t, "TEST", st.Notification.Payload.NotificationType)
	assert.Equal(t, signed, st.SignedPayload)
}

func TestTestNotification_StatusNotReadyThenReady(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(404, 4040008, "not yet"))
	c, _ := f.client()
	_, err := c.GetTestNotificationStatus(ctx, "tok")
	assert.ErrorIs(t, err, ErrTestNotificationNotReady)
	assert.ErrorIs(t, err, entitlements.ErrNotFound)
}

func TestTestNotification_NoURLConfigured(t *testing.T) {
	f := newFakeApple(t)
	f.respond(apiErr(404, 4040007, "No App Store Server Notification URL found"))
	c, _ := f.client()
	_, err := c.RequestTestNotification(ctx)
	assert.ErrorIs(t, err, ErrNotificationURLNotFound)
}

func TestTestNotification_EmptyTokenIsAnError(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{}))
	c, _ := f.client()
	_, err := c.RequestTestNotification(ctx)
	require.ErrorContains(t, err, "no testNotificationToken")
}

func TestTestNotification_UnverifiablePayloadIsNotDecoded(t *testing.T) {
	f := newFakeApple(t)
	rogue := testutil.NewAppleCA(t)
	f.respond(ok(map[string]any{
		"signedPayload":          notificationSigned(t, rogue, "TEST", "", "u", 1, nil, 0),
		"firstSendAttemptResult": "SUCCESS",
	}))
	c, _ := f.client()
	st, err := c.GetTestNotificationStatus(ctx, "tok")
	require.NoError(t, err, "delivery info is still useful")
	assert.Nil(t, st.Notification)
	assert.ErrorIs(t, st.VerifyErr, entitlements.ErrInvalidSignature)
	assert.Equal(t, "SUCCESS", st.FirstSendAttemptResult)
}

func TestGetNotificationHistory_RequestBodyAndRecords(t *testing.T) {
	f := newFakeApple(t)
	good := notificationSigned(t, f.ca, "DID_RENEW", "", "uuid-1", 1791633600000, tx("t2", nil), 1)
	bad := notificationSigned(t, testutil.NewAppleCA(t), "DID_RENEW", "", "uuid-2", 1791633700000, tx("t3", nil), 1)
	f.respond(ok(map[string]any{
		"notificationHistory": []map[string]any{historyRecord(good), historyRecord(bad)},
		"hasMore":             true, "paginationToken": "pt-1",
	}))
	c, _ := f.client()
	start, end := time.UnixMilli(1790000000000), time.UnixMilli(1791700000000)

	page, err := c.GetNotificationHistory(ctx, NotificationHistoryRequest{
		Start: start, End: end, NotificationType: "DID_RENEW", NotificationSubtype: "X", OnlyFailures: true,
	}, "pt-0")
	require.NoError(t, err)

	r := f.requests()[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/inApps/v1/notifications/history", r.Path)
	assert.Equal(t, []string{"pt-0"}, r.Query["paginationToken"])
	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &body))
	assert.Equal(t, map[string]any{
		"startDate": 1790000000000.0, "endDate": 1791700000000.0, "notificationType": "DID_RENEW",
		"notificationSubtype": "X", "onlyFailures": true,
	}, body, "dates are UNIX milliseconds; unset filters are omitted")

	assert.True(t, page.HasMore)
	assert.Equal(t, "pt-1", page.PaginationToken)
	require.Len(t, page.Records, 2)
	assert.Equal(t, good, page.Records[0].SignedPayload)
	require.NoError(t, page.Records[0].VerifyErr)
	assert.Equal(t, "uuid-1", page.Records[0].Notification.Payload.NotificationUUID)
	assert.Equal(t, "t2", page.Records[0].Notification.Transaction.TransactionID)
	assert.Equal(t, "SUCCESS", page.Records[0].FirstSendAttemptResult)
	assert.Nil(t, page.Records[1].Notification, "an unverifiable record is not decoded")
	assert.ErrorIs(t, page.Records[1].VerifyErr, entitlements.ErrInvalidSignature)
	assert.Equal(t, bad, page.Records[1].SignedPayload, "but its raw payload is kept for a parser with its own clock")
}

func TestGetNotificationHistory_TransactionFilterAndValidation(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"notificationHistory": []any{}}))
	c, _ := f.client()
	_, err := c.GetNotificationHistory(ctx, NotificationHistoryRequest{Start: time.UnixMilli(1), End: time.UnixMilli(2), TransactionID: "T1"}, "")
	require.NoError(t, err)
	r := f.requests()[0]
	assert.Empty(t, r.Query)
	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &body))
	assert.Equal(t, "T1", body["transactionId"])
	assert.NotContains(t, body, "onlyFailures")

	_, err = c.GetNotificationHistory(ctx, NotificationHistoryRequest{Start: time.UnixMilli(1)}, "")
	require.ErrorContains(t, err, "Start and End")
	assert.Len(t, f.requests(), 1)
}

func TestNotificationHistory_WalksAllPagesAndKeepsTheRequest(t *testing.T) {
	f := newFakeApple(t)
	rec := func(n int) map[string]any {
		return historyRecord(notificationSigned(t, f.ca, "TEST", "", "u"+strconv.Itoa(n), int64(n), nil, 0))
	}
	f.script(
		ok(map[string]any{"notificationHistory": []map[string]any{rec(1), rec(2)}, "hasMore": true, "paginationToken": "a"}),
		ok(map[string]any{"notificationHistory": []map[string]any{rec(3)}, "hasMore": true, "paginationToken": "b"}),
		ok(map[string]any{"notificationHistory": []map[string]any{rec(4)}, "hasMore": false}),
	)
	c, _ := f.client()
	req := NotificationHistoryRequest{Start: time.UnixMilli(1), End: time.UnixMilli(9), OnlyFailures: true}

	var uuids []string
	for r, err := range c.NotificationHistory(ctx, req) {
		require.NoError(t, err)
		uuids = append(uuids, r.Notification.Payload.NotificationUUID)
	}
	assert.Equal(t, []string{"u1", "u2", "u3", "u4"}, uuids)

	reqs := f.requests()
	require.Len(t, reqs, 3)
	assert.Empty(t, reqs[0].Query)
	assert.Equal(t, []string{"a"}, reqs[1].Query["paginationToken"])
	assert.Equal(t, []string{"b"}, reqs[2].Query["paginationToken"])
	for _, r := range reqs {
		assert.JSONEq(t, string(reqs[0].Body), string(r.Body), "same body on every page, as Apple requires")
	}
}

func TestNotificationHistory_ExpiredTokenAndStuckCursor(t *testing.T) {
	f := newFakeApple(t)
	f.script(
		ok(map[string]any{"hasMore": true, "paginationToken": "a"}),
		apiErr(400, 4000017, "expired"),
	)
	c, _ := f.client()
	var last error
	for _, err := range c.NotificationHistory(ctx, NotificationHistoryRequest{Start: time.UnixMilli(1), End: time.UnixMilli(2)}) {
		last = err
	}
	assert.ErrorIs(t, last, ErrPaginationTokenExpired)

	f2 := newFakeApple(t)
	f2.respond(ok(map[string]any{"hasMore": true, "paginationToken": "same"}))
	c2, _ := f2.client()
	last = nil
	for _, err := range c2.NotificationHistory(ctx, NotificationHistoryRequest{Start: time.UnixMilli(1), End: time.UnixMilli(2)}) {
		last = err
	}
	assert.ErrorIs(t, last, errPagingStuck)
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{
		StatusActive: "active", StatusExpired: "expired", StatusBillingRetry: "billing_retry",
		StatusGracePeriod: "grace_period", StatusRevoked: "revoked", Status(0): "unknown", Status(9): "unknown",
	} {
		assert.Equal(t, want, s.String())
	}
}

func TestProductionClientRejectsSandboxData(t *testing.T) {
	f := newFakeApple(t)
	f.respond(ok(map[string]any{"signedTransactionInfo": f.ca.Sign(t, tx("1", nil))})) // environment Sandbox
	c, _ := f.clientEnv(entitlements.EnvironmentProduction)
	_, err := c.GetTransactionInfo(ctx, "1")
	assert.ErrorIs(t, err, ErrIdentityMismatch)
}
