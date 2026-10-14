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
// In the grace period Play "dynamically extends the expiryTime value until the
// grace period has expired"
// (https://developer.android.com/google/play/billing/lifecycle/subscriptions),
// so expiryTime is already the end of the grace period and is used as is:
// no floor, no extension. If it is not in the future, entitlements.Entitlement.Active
// is false. A grace subscription without expiryTime returns ErrNoExpiry.
//
// # Upgrades
//
// linkedPurchaseToken becomes Event.SupersedesTxID only when the new purchase
// grants access (active, canceled, grace). A canceled pending upgrade, or an
// expired or held link, never retires the old chain.
//
// # Voided purchases
//
// A voidedPurchaseNotification maps to refunded only for productType 1
// (subscription) and a full refund (refundType 1); one-time products
// (productType 2) and quantity-based partial refunds (refundType 2) return
// entitlements.ErrIgnored. Absent fields count as subscription / full refund
// (https://developer.android.com/google/play/billing/rtdn-reference).
//
// The push is not trusted on its own: the parser fetches the subscription and
// applies the void only if the voided orderId is the latest successful order
// of a line item (a void of an older renewal order does not end a running
// subscription). When Google returns no order ids, the void applies only if
// the subscription no longer grants access. Otherwise Parse returns
// entitlements.ErrIgnored.
//
// # Input validation
//
// NewParser requires the package name: other packages are rejected before
// anything is fetched. Control characters in messageId or purchaseToken, and an
// eventTimeMillis more than WithMaxFutureSkew (default 5 minutes) ahead of the
// clock, return ErrMalformed. Purchase tokens are secrets: errors from the
// fetcher have them replaced by "[redacted]" (see RedactToken).
//
// # Test purchases
//
// subscriptionsv2 marks license-tester purchases with an empty testPurchase
// object. Parse sets Event.Environment to entitlements.EnvironmentGoogleTest
// for them, which Service ignores unless built WithAllowSandbox. Use
// WithRejectTestPurchases to refuse them in the parser with ErrTestPurchase.
//
// # User identification
//
// UserID is obfuscatedExternalAccountId, falling back to
// obfuscatedExternalProfileId when the account id is absent.
//
// # Fetch errors
//
// Errors from the fetcher carry one class (the original error stays
// reachable): ErrPurchaseNotFound for HTTP 404/410, ErrPurchaseInvalid for 400,
// ErrFetchConfig for 401/403,
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
