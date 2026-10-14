// Package google decodes Google Play Real-Time Developer Notifications
// delivered over Pub/Sub push and maps them to entitlements.Event.
//
// The notification type is only a hint. For subscription notifications the
// parser calls purchases.subscriptionsv2.get and maps the fetched
// subscriptionState.
//
// # Acknowledgement
//
// This package never acknowledges purchases. Google refunds a subscription
// that is not acknowledged within 3 days, so the caller (the app via Play
// Billing, or the backend through purchases.subscriptions.acknowledge) is
// responsible. SubscriptionPurchaseV2.AcknowledgementState is exposed for that.
//
// # Expiry
//
// lineItems[].expiryTime is "the time at which the subscription expired or
// will expire unless the access is extended"
// (https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2).
// It is not documented as required. A line item without it (a deferred
// replacement that has not taken effect, deferredItemReplacement) is skipped
// when choosing the latest expiry. If a subscription that grants access
// (active or canceled) has no expiry at all, Parse returns ErrNoExpiry rather
// than granting open-ended access.
//
// In the grace period the reference documents no grace end time
// (InGracePeriodStateContext only has renewalDeclined), so expiryTime may
// already be in the past while Google still grants access. The parser sets
// ExpiresAt to max(expiryTime, notification time + grace duration); see
// WithGraceDuration and DefaultGraceDuration. This is a bound, not Google's
// exact value: verify against a live grace-period response, and rely on the
// following ON_HOLD/EXPIRED notification to close the window.
//
// # Voided purchases
//
// A voidedPurchaseNotification maps to refunded only for productType 1
// (subscription) and a full refund (refundType 1). One-time products
// (productType 2) and quantity-based partial refunds (refundType 2) return
// entitlements.ErrIgnored. Absent fields count as subscription / full refund
// (https://developer.android.com/google/play/billing/rtdn-reference).
//
// # Test purchases
//
// subscriptionsv2 marks license-tester purchases with an empty testPurchase
// object. Parse sets Event.Sandbox for them. By default they are processed
// like real ones so QA works against production; use WithRejectTestPurchases
// to refuse them with ErrTestPurchase.
//
// # User identification
//
// UserID is obfuscatedExternalAccountId, falling back to
// obfuscatedExternalProfileId when the account id is absent.
//
// # Fetch errors
//
// Errors from the fetcher carry one class (the original error stays
// reachable): ErrPurchaseNotFound for HTTP 404/410, ErrFetchConfig for 401/403,
// ErrFetchTransient for everything else. Package google/androidpublisher
// classifies the Play API errors; the parser marks any unclassified fetcher
// error, including a timeout, as transient. httpx maps the classes to 200,
// 500 with a loud log, and 500. Each fetch is bounded by WithFetchTimeout.
//
// # Dependencies
//
// This package does not import the Google API client. Use
// google/androidpublisher for the real fetcher.
//
// # Concurrency
//
// Parser is safe for concurrent use as long as the fetcher is.
package google
