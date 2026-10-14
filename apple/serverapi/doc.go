// Package serverapi is a client for Apple's App Store Server API and the glue
// that turns its answers into entitlements events.
//
// Webhooks (package apple) get lost: your server was down, a deploy dropped a
// request, a notification was rejected. The Server API is the source of truth
// to repair that. Reconcile asks Apple for a customer's current subscription
// status and feeds it to the Service; ReplayNotificationHistory re-feeds the
// notifications Apple says it sent.
//
// Every signed value the API returns (signedTransactionInfo,
// signedRenewalInfo, signedTransactions, signedPayload) is verified with
// apple.Verifier and pinned to the client's bundle ID and environment before
// it is used; nothing is decoded blindly.
//
// Authentication: each request carries an ES256 JWT with header {alg, kid,
// typ: JWT} and claims {iss: issuer ID, iat, exp (at most 60 minutes after
// iat), aud: appstoreconnect-v1, bid: bundle ID}, signed with the .p8 key from
// App Store Connect. A new token is signed per request. The key and tokens are
// never logged, and Config, Client print a placeholder instead of the key.
//
// Retries: HTTP 429, 5xx, transport errors and Apple's "please try again"
// error codes are retried with jittered exponential backoff. Apple sends
// Retry-After as a UNIX time in milliseconds (the standard seconds and date
// forms are accepted too). Rate limits are enforced per hour, so a Retry-After
// longer than WithMaxRetryWait is returned to the caller instead of slept on.
//
// Errors are *APIError values built from Apple's {errorCode, errorMessage}
// body; match them with errors.Is against the sentinels in this package (all
// 404s also match entitlements.ErrNotFound).
//
// Environments: Production is https://api.storekit.apple.com and Sandbox is
// https://api.storekit-sandbox.apple.com. A transaction made in one is
// unknown ("transaction id not found") in the other. Sandbox rate limits are
// 10% of production's.
//
// Apple documentation, checked against the live pages on 2026-10-15:
//   - JWT: https://developer.apple.com/documentation/appstoreserverapi/generating-json-web-tokens-for-api-requests
//   - Rate limits and Retry-After: https://developer.apple.com/documentation/appstoreserverapi/identifying-rate-limits
//   - Error codes: https://developer.apple.com/documentation/appstoreserverapi/error-codes
//   - Get All Subscription Statuses: https://developer.apple.com/documentation/appstoreserverapi/get-all-subscription-statuses
//   - Get Transaction History: https://developer.apple.com/documentation/appstoreserverapi/get-transaction-history
//   - Get Transaction Info: https://developer.apple.com/documentation/appstoreserverapi/get-transaction-info
//   - Look Up Order ID: https://developer.apple.com/documentation/appstoreserverapi/look-up-order-id
//   - Request a Test Notification: https://developer.apple.com/documentation/appstoreserverapi/request-a-test-notification
//   - Get Test Notification Status: https://developer.apple.com/documentation/appstoreserverapi/get-test-notification-status
//   - Get Notification History: https://developer.apple.com/documentation/appstoreserverapi/get-notification-history
//   - Subscription status values: https://developer.apple.com/documentation/appstoreserverapi/status
package serverapi
