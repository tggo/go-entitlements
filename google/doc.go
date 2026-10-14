// Package google decodes Google Play Real-Time Developer Notifications
// delivered over Pub/Sub push and maps them to entitlements.Event.
//
// The notification type is only a hint. For subscription notifications the
// parser calls purchases.subscriptionsv2.get and maps the fetched
// subscriptionState.
//
// # Acknowledgement
//
// Google refunds a subscription that is not acknowledged within 3 days.
// Parser.ParseResult reports in Result.Ack the purchases that are still
// pending (acknowledgementState ACKNOWLEDGEMENT_STATE_PENDING) and grant
// access (active, canceled, grace). Acknowledger is the interface that does the
// call; google/androidpublisher implements it with
// purchases.subscriptions.acknowledge (subscriptionsv2 has no acknowledge
// method; the subscription id in its path is optional since 2025-05-21, and
// the notification's subscriptionId is used, or the first item's product for add-on purchases whose RTDN has none). Acknowledge only after the event
// is stored, and treat ErrAlreadyAcknowledged, ErrPurchaseNotFound and
// ErrPurchaseInvalid as final. httpx.WithAcknowledger wires this up and retries
// through Pub/Sub redelivery. Test purchases are acknowledged too.
// See https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions/acknowledge.
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
// # Add-ons
//
// A subscription with add-ons is one purchase token with several line items,
// each with its own expiryTime, and its RTDN carries no subscriptionId
// (https://developer.android.com/google/play/billing/subscription-with-addons;
// lineItems in
// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2).
// Each line item becomes its own entitlement, so IsActive(user, product) is
// right per product. ParseAll returns one Event per item. A purchase that
// yields exactly one item is keyed by the plain purchase token, as before add-ons
// were supported, so existing stored rows stay valid. With several items the
// key is purchaseToken + "#" + productId and the event ID is messageId + "#" +
// productId. The product, not a position, is in the key so that adding,
// removing or reordering items does not rebind a row to another product.
//
// An item without expiryTime (a deferred replacement that has not taken effect
// yet) grants nothing and yields no event. A removed add-on "expires at the end
// of its current billing period" (same page): with several items an active item
// whose expiryTime has passed yields no event, because its stored row already
// ends at that time. When a purchase changes shape (one item gains an add-on,
// or loses it) the row under the old key shape is not rewritten; it ends at its
// own stored expiryTime. Immediate item-based revocation is not tracked.
//
// Parse returns ErrMultipleEvents instead of silently dropping items; use
// ParseAll or ParseResult.
//
// # Upgrades
//
// linkedPurchaseToken becomes Event.SupersedesTxID only when the new purchase
// grants access (active, canceled, grace). A canceled pending upgrade, or an
// expired or held link, never retires the old chain. The replaced token is
// fetched (subscriptionsv2.get) to learn its keys: a replaced add-on
// subscription is retired item by item, and the chain is followed through
// linkedPurchaseToken (at most MaxLinkedTokens tokens, cycles stop it), so
// retiring C (linked B, linked A) also retires A when only C's notification
// arrived. A replaced token that Google no longer knows (404, 410, 400) is
// retired by its plain key; a transient failure is returned so the push is
// retried. The first replaced token's plain key is retired through
// SupersedesTxID, which also hands its user to a new token without account ids.
//
// # Syncing from a purchase token
//
// ParseToken maps the current state of a token the app sends right after a
// purchase, so access does not wait for the push. Its event IDs are digests of
// the state, so repeating a sync dedupes, and its OccurredAt is the fetch time
// minus the clock-skew margin, so a sync cannot overwrite a push that is newer
// than the state it fetched.
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
// applies the void to every key of the purchase (the plain token and, with
// several items, each item) only if the voided orderId is the latest successful order
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
