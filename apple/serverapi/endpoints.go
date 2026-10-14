package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/apple"
)

// ErrIdentityMismatch means a response, or a signed value inside it, names
// another bundle ID or environment than the Client was built for. It wraps
// entitlements.ErrInvalidSignature: never grant access from such a value.
var ErrIdentityMismatch = fmt.Errorf("serverapi: signed value belongs to another app or environment: %w", entitlements.ErrInvalidSignature)

func mismatch(what, got, want string) error {
	return fmt.Errorf("%w: %s is %q, want %q", ErrIdentityMismatch, what, got, want)
}

// checkIdentity compares the bundle ID and environment a response claims.
func (c *Client) checkIdentity(what, bundleID, env string) error {
	if bundleID != c.bundleID {
		return mismatch(what+" bundleId", bundleID, c.bundleID)
	}
	if entitlements.Environment(env) != c.env {
		return mismatch(what+" environment", env, string(c.env))
	}
	return nil
}

// verifyTransaction verifies a signedTransactionInfo JWS and pins it to the
// client's app and environment.
func (c *Client) verifyTransaction(signed string) (*apple.JWSTransactionDecodedPayload, error) {
	var tx apple.JWSTransactionDecodedPayload
	if err := c.verifier.Verify(signed, &tx); err != nil {
		return nil, fmt.Errorf("signedTransactionInfo: %w", err)
	}
	if err := c.checkIdentity("signedTransactionInfo", tx.BundleID, tx.Environment); err != nil {
		return nil, err
	}
	return &tx, nil
}

func (c *Client) verifyRenewal(signed string) (*apple.JWSRenewalInfoDecodedPayload, error) {
	var ri apple.JWSRenewalInfoDecodedPayload
	if err := c.verifier.Verify(signed, &ri); err != nil {
		return nil, fmt.Errorf("signedRenewalInfo: %w", err)
	}
	// Renewal info carries no bundle ID; only the environment can be pinned.
	if entitlements.Environment(ri.Environment) != c.env {
		return nil, mismatch("signedRenewalInfo environment", ri.Environment, string(c.env))
	}
	return &ri, nil
}

func (c *Client) verifyTransactions(signed []string) ([]apple.JWSTransactionDecodedPayload, error) {
	out := make([]apple.JWSTransactionDecodedPayload, 0, len(signed))
	for i, s := range signed {
		tx, err := c.verifyTransaction(s)
		if err != nil {
			return nil, fmt.Errorf("signedTransactions[%d]: %w", i, err)
		}
		out = append(out, *tx)
	}
	return out, nil
}

// verifyNotification verifies a signedPayload (the notification JWS and the
// transaction and renewal JWS nested in it).
func (c *Client) verifyNotification(signed string) (*apple.Notification, error) {
	body, err := json.Marshal(apple.ResponseBodyV2{SignedPayload: signed})
	if err != nil {
		return nil, err
	}
	return c.verifier.VerifyNotification(body)
}

func requireID(what, id string) error {
	if id == "" {
		return fmt.Errorf("serverapi: %s is required", what)
	}
	return nil
}

// GetAllSubscriptionStatuses returns the status of every auto-renewable
// subscription of the customer who owns anyTransactionID (an
// originalTransactionId, transactionId or appTransactionId), grouped by
// subscription group. With statuses given, only subscriptions in one of those
// states are returned. Every signed value is verified and pinned to the
// client's bundle ID and environment.
//
//	GET /inApps/v1/subscriptions/{anyTransactionId}
//	https://developer.apple.com/documentation/appstoreserverapi/get-all-subscription-statuses
func (c *Client) GetAllSubscriptionStatuses(ctx context.Context, anyTransactionID string, statuses ...Status) (*SubscriptionStatuses, error) {
	if err := requireID("transaction ID", anyTransactionID); err != nil {
		return nil, err
	}
	var q url.Values
	for _, s := range statuses {
		if q == nil {
			q = url.Values{}
		}
		q.Add("status", strconv.Itoa(int(s)))
	}
	var resp struct {
		Data []struct {
			SubscriptionGroupIdentifier string `json:"subscriptionGroupIdentifier"`
			LastTransactions            []struct {
				OriginalTransactionID string `json:"originalTransactionId"`
				Status                int    `json:"status"`
				SignedTransactionInfo string `json:"signedTransactionInfo"`
				SignedRenewalInfo     string `json:"signedRenewalInfo"`
			} `json:"lastTransactions"`
		} `json:"data"`
		Environment string `json:"environment"`
		AppAppleID  int64  `json:"appAppleId"`
		BundleID    string `json:"bundleId"`
	}
	if err := c.do(ctx, http.MethodGet, "/inApps/v1/subscriptions/"+url.PathEscape(anyTransactionID), q, nil, &resp); err != nil {
		return nil, err
	}
	if err := c.checkIdentity("response", resp.BundleID, resp.Environment); err != nil {
		return nil, err
	}
	out := &SubscriptionStatuses{Environment: c.env, BundleID: resp.BundleID, AppAppleID: resp.AppAppleID}
	for _, g := range resp.Data {
		grp := SubscriptionGroup{SubscriptionGroupIdentifier: g.SubscriptionGroupIdentifier}
		for _, lt := range g.LastTransactions {
			tx, err := c.verifyTransaction(lt.SignedTransactionInfo)
			if err != nil {
				return nil, fmt.Errorf("subscription %s: %w", lt.OriginalTransactionID, err)
			}
			item := SubscriptionStatus{
				OriginalTransactionID: lt.OriginalTransactionID,
				Status:                Status(lt.Status),
				Transaction:           *tx,
				SignedTransactionInfo: lt.SignedTransactionInfo,
				SignedRenewalInfo:     lt.SignedRenewalInfo,
			}
			if item.OriginalTransactionID == "" {
				item.OriginalTransactionID = tx.OriginalTransactionID
			}
			if lt.SignedRenewalInfo != "" {
				if item.Renewal, err = c.verifyRenewal(lt.SignedRenewalInfo); err != nil {
					return nil, fmt.Errorf("subscription %s: %w", lt.OriginalTransactionID, err)
				}
			}
			grp.Subscriptions = append(grp.Subscriptions, item)
		}
		out.Groups = append(out.Groups, grp)
	}
	return out, nil
}

// GetTransactionHistory returns one page (up to 20 transactions) of the
// customer's history, in every state including refunded and revoked. Pass the
// previous page's Revision (empty for the first page) together with the same
// query. TransactionHistory walks all pages.
//
//	GET /inApps/v2/history/{anyTransactionId}
//	https://developer.apple.com/documentation/appstoreserverapi/get-transaction-history
func (c *Client) GetTransactionHistory(ctx context.Context, anyTransactionID string, q HistoryQuery, revision string) (*HistoryPage, error) {
	if err := requireID("transaction ID", anyTransactionID); err != nil {
		return nil, err
	}
	var resp struct {
		AppAppleID         int64    `json:"appAppleId"`
		BundleID           string   `json:"bundleId"`
		Environment        string   `json:"environment"`
		HasMore            bool     `json:"hasMore"`
		Revision           string   `json:"revision"`
		SignedTransactions []string `json:"signedTransactions"`
	}
	if err := c.do(ctx, http.MethodGet, "/inApps/v2/history/"+url.PathEscape(anyTransactionID), q.values(revision), nil, &resp); err != nil {
		return nil, err
	}
	if err := c.checkIdentity("response", resp.BundleID, resp.Environment); err != nil {
		return nil, err
	}
	txs, err := c.verifyTransactions(resp.SignedTransactions)
	if err != nil {
		return nil, err
	}
	return &HistoryPage{
		Environment: c.env, BundleID: resp.BundleID, AppAppleID: resp.AppAppleID,
		HasMore: resp.HasMore, Revision: resp.Revision, Transactions: txs,
	}, nil
}

func (q HistoryQuery) values(revision string) url.Values {
	v := url.Values{}
	if revision != "" {
		v.Set("revision", revision)
	}
	if !q.StartDate.IsZero() {
		v.Set("startDate", strconv.FormatInt(q.StartDate.UnixMilli(), 10))
	}
	if !q.EndDate.IsZero() {
		v.Set("endDate", strconv.FormatInt(q.EndDate.UnixMilli(), 10))
	}
	for _, s := range q.ProductIDs {
		v.Add("productId", s)
	}
	for _, s := range q.ProductTypes {
		v.Add("productType", s)
	}
	if q.InAppOwnershipType != "" {
		v.Set("inAppOwnershipType", q.InAppOwnershipType)
	}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	if q.Revoked != nil {
		v.Set("revoked", strconv.FormatBool(*q.Revoked))
	}
	for _, s := range q.SubscriptionGroupIDs {
		v.Add("subscriptionGroupIdentifier", s)
	}
	return v
}

// errPagingStuck means Apple said there is more but gave no new cursor;
// following it would loop forever.
var errPagingStuck = errors.New("serverapi: server reports more pages but returned no new cursor")

// TransactionHistory iterates over the customer's whole history, page after
// page. The iteration stops at the first error, which is yielded once.
func (c *Client) TransactionHistory(ctx context.Context, anyTransactionID string, q HistoryQuery) iter.Seq2[apple.JWSTransactionDecodedPayload, error] {
	return func(yield func(apple.JWSTransactionDecodedPayload, error) bool) {
		revision := ""
		for {
			page, err := c.GetTransactionHistory(ctx, anyTransactionID, q, revision)
			if err != nil {
				yield(apple.JWSTransactionDecodedPayload{}, err)
				return
			}
			for _, tx := range page.Transactions {
				if !yield(tx, nil) {
					return
				}
			}
			if !page.HasMore {
				return
			}
			if page.Revision == "" || page.Revision == revision {
				yield(apple.JWSTransactionDecodedPayload{}, errPagingStuck)
				return
			}
			revision = page.Revision
		}
	}
}

// GetTransactionInfo returns the verified transaction with the given ID
// (any in-app purchase type; an originalTransactionId works too).
//
//	GET /inApps/v1/transactions/{transactionId}
//	https://developer.apple.com/documentation/appstoreserverapi/get-transaction-info
func (c *Client) GetTransactionInfo(ctx context.Context, transactionID string) (*apple.JWSTransactionDecodedPayload, error) {
	if err := requireID("transaction ID", transactionID); err != nil {
		return nil, err
	}
	var resp struct {
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	}
	if err := c.do(ctx, http.MethodGet, "/inApps/v1/transactions/"+url.PathEscape(transactionID), nil, nil, &resp); err != nil {
		return nil, err
	}
	return c.verifyTransaction(resp.SignedTransactionInfo)
}

// LookUpOrderID returns the in-app purchases of an order ID the customer
// read from an App Store receipt email. An unknown order is not an error:
// OrderLookup.Valid is false. Apple documents this endpoint for the
// production host only.
//
//	GET /inApps/v1/lookup/{orderId}
//	https://developer.apple.com/documentation/appstoreserverapi/look-up-order-id
func (c *Client) LookUpOrderID(ctx context.Context, orderID string) (*OrderLookup, error) {
	if err := requireID("order ID", orderID); err != nil {
		return nil, err
	}
	var resp struct {
		Status             int      `json:"status"`
		SignedTransactions []string `json:"signedTransactions"`
	}
	if err := c.do(ctx, http.MethodGet, "/inApps/v1/lookup/"+url.PathEscape(orderID), nil, nil, &resp); err != nil {
		return nil, err
	}
	txs, err := c.verifyTransactions(resp.SignedTransactions)
	if err != nil {
		return nil, err
	}
	return &OrderLookup{Valid: resp.Status == 0, Transactions: txs}, nil
}

// RequestTestNotification asks the App Store to send a TEST notification to
// the notification URL configured for the client's environment, and returns
// the token for GetTestNotificationStatus.
//
//	POST /inApps/v1/notifications/test
//	https://developer.apple.com/documentation/appstoreserverapi/request-a-test-notification
func (c *Client) RequestTestNotification(ctx context.Context) (token string, err error) {
	var resp struct {
		TestNotificationToken string `json:"testNotificationToken"`
	}
	if err = c.do(ctx, http.MethodPost, "/inApps/v1/notifications/test", nil, nil, &resp); err != nil {
		return "", err
	}
	if resp.TestNotificationToken == "" {
		return "", errors.New("serverapi: response has no testNotificationToken")
	}
	return resp.TestNotificationToken, nil
}

type wireSendAttempt struct {
	AttemptDate       int64  `json:"attemptDate"`
	SendAttemptResult string `json:"sendAttemptResult"`
}

func sendAttempts(in []wireSendAttempt) []SendAttempt {
	out := make([]SendAttempt, len(in))
	for i, a := range in {
		out[i] = SendAttempt{AttemptDate: time.UnixMilli(a.AttemptDate).UTC(), Result: a.SendAttemptResult}
	}
	return out
}

// GetTestNotificationStatus reports how the delivery of a test notification
// went. Right after RequestTestNotification Apple answers
// ErrTestNotificationNotReady until the first attempt is recorded; poll. The
// status is kept for six months.
//
//	GET /inApps/v1/notifications/test/{testNotificationToken}
//	https://developer.apple.com/documentation/appstoreserverapi/get-test-notification-status
func (c *Client) GetTestNotificationStatus(ctx context.Context, token string) (*TestNotificationStatus, error) {
	if err := requireID("test notification token", token); err != nil {
		return nil, err
	}
	var resp struct {
		SignedPayload          string            `json:"signedPayload"`
		FirstSendAttemptResult string            `json:"firstSendAttemptResult"`
		SendAttempts           []wireSendAttempt `json:"sendAttempts"`
	}
	if err := c.do(ctx, http.MethodGet, "/inApps/v1/notifications/test/"+url.PathEscape(token), nil, nil, &resp); err != nil {
		return nil, err
	}
	st := &TestNotificationStatus{
		FirstSendAttemptResult: resp.FirstSendAttemptResult,
		SendAttempts:           sendAttempts(resp.SendAttempts),
		SignedPayload:          resp.SignedPayload,
	}
	if resp.SignedPayload != "" {
		st.Notification, st.VerifyErr = c.verifyNotification(resp.SignedPayload)
	}
	return st, nil
}

// GetNotificationHistory returns one page (up to 20 records) of the
// notifications the App Store tried to send to your server: 180 days back in
// production, 30 in the sandbox. Pass the previous page's PaginationToken
// (empty for the first page) with the same request. NotificationHistory
// walks all pages.
//
//	POST /inApps/v1/notifications/history
//	https://developer.apple.com/documentation/appstoreserverapi/get-notification-history
func (c *Client) GetNotificationHistory(ctx context.Context, req NotificationHistoryRequest, paginationToken string) (*NotificationHistoryPage, error) {
	if req.Start.IsZero() || req.End.IsZero() {
		return nil, errors.New("serverapi: notification history needs Start and End")
	}
	body := struct {
		StartDate           int64  `json:"startDate"`
		EndDate             int64  `json:"endDate"`
		NotificationType    string `json:"notificationType,omitempty"`
		NotificationSubtype string `json:"notificationSubtype,omitempty"`
		OnlyFailures        bool   `json:"onlyFailures,omitempty"`
		TransactionID       string `json:"transactionId,omitempty"`
	}{req.Start.UnixMilli(), req.End.UnixMilli(), req.NotificationType, req.NotificationSubtype, req.OnlyFailures, req.TransactionID}
	var q url.Values
	if paginationToken != "" {
		q = url.Values{"paginationToken": {paginationToken}}
	}
	var resp struct {
		NotificationHistory []struct {
			SignedPayload          string            `json:"signedPayload"`
			FirstSendAttemptResult string            `json:"firstSendAttemptResult"`
			SendAttempts           []wireSendAttempt `json:"sendAttempts"`
		} `json:"notificationHistory"`
		HasMore         bool   `json:"hasMore"`
		PaginationToken string `json:"paginationToken"`
	}
	if err := c.do(ctx, http.MethodPost, "/inApps/v1/notifications/history", q, body, &resp); err != nil {
		return nil, err
	}
	page := &NotificationHistoryPage{HasMore: resp.HasMore, PaginationToken: resp.PaginationToken}
	for _, r := range resp.NotificationHistory {
		rec := NotificationRecord{
			SignedPayload:          r.SignedPayload,
			FirstSendAttemptResult: r.FirstSendAttemptResult,
			SendAttempts:           sendAttempts(r.SendAttempts),
		}
		rec.Notification, rec.VerifyErr = c.verifyNotification(r.SignedPayload)
		page.Records = append(page.Records, rec)
	}
	return page, nil
}

// NotificationHistory iterates over every record matching req, page after
// page. The iteration stops at the first error, which is yielded once.
func (c *Client) NotificationHistory(ctx context.Context, req NotificationHistoryRequest) iter.Seq2[NotificationRecord, error] {
	return func(yield func(NotificationRecord, error) bool) {
		token := ""
		for {
			page, err := c.GetNotificationHistory(ctx, req, token)
			if err != nil {
				yield(NotificationRecord{}, err)
				return
			}
			for _, r := range page.Records {
				if !yield(r, nil) {
					return
				}
			}
			if !page.HasMore {
				return
			}
			if page.PaginationToken == "" || page.PaginationToken == token {
				yield(NotificationRecord{}, errPagingStuck)
				return
			}
			token = page.PaginationToken
		}
	}
}
