# Prior art: heroiclabs/nakama

Subscription and IAP bugs in [heroiclabs/nakama](https://github.com/heroiclabs/nakama) (Go game server, Apache-2.0) and how this library fares against each. It extends `README.md` in this folder; the 21 libraries covered there are not repeated.

Method, 2026-10-15, nakama `master` at `9f0a9127e2e9a7143e57eae10f3cd813878eff3d` (2026-10-08):

- `gh` listing of all issues (614) and PRs (961), open and closed. Keyword filter on title and the first 3,000 characters of the body (subscription, notification, RTDN, apple, google play, purchase, refund, receipt, iap, storekit, entitlement, expire, grace, upgrade, linkedpurchasetoken, huawei, instant): 83 issues and 132 PRs matched. Of those, 22 issues and 50 PRs were relevant and read in full with comments (the rest are friends/notifications/Apple Sign-In/console noise).
- 78 unique commits touching `server/core_subscription.go`, `server/core_purchase.go`, `iap/` and the deleted `server/google_refund_scheduler.go`. 26 diffs read in full or in the relevant hunks; the rest (dependency bumps, lint, console views, Facebook/Huawei/Samsung validation, pagination, proto regeneration) were triaged by subject.
- `server/core_subscription.go` (1,702 lines) and `iap/iap.go` (1,016 lines) read in full at the commit above.
- Our side was checked by reading the code and by throwaway experiments in a copy of the repository (nothing in this repository was changed). "Reproduced" below means such an experiment was run.

The issue discussion in nakama is thin: most fixes landed without a report, and most bugs below were found in the code or the diffs. Where a finding has no upstream issue it says "analysis".

## 1. Design comparison

| Topic | Nakama | go-entitlements |
|---|---|---|
| Entry points | Client-initiated validation (`ValidateSubscriptionApple/Google`, session user) plus notification webhooks | Webhooks only. User comes from `appAccountToken` / `obfuscatedExternalAccountId` |
| Apple JWS trust | `iap.ValidateAppleJwsSignature` (`iap/iap.go:922`): x5c needs 3 certs, leaf verified against embedded Apple root, ECDSA signature checked. No marker OID check, no `alg` check, no bundle ID or environment check anywhere (`grep -i bundle` finds only struct fields) | Chain to Root CA G3, both Apple marker OIDs, `alg` pinned to ES256, bundle ID required, environment pinned optionally, nested payloads checked against the outer one (`apple/verifier.go:181-195`, `290-325`) |
| JWS segment encoding | `base64.RawStdEncoding` on JWS segments (`core_subscription.go:284,1245,1263,1281`); JWS is base64url | `RawURLEncoding` (`apple/verifier.go:218`) |
| Google push auth | None. The only protection is a secret path segment, `/v2/console/google/notifications/<notifications_endpoint_id>` (`server/console.go:228-234`). Apple endpoint the same, which is fine since the JWS is signed | Pub/Sub OIDC token (audience + service account) mandatory, constructor refuses to start without it |
| Google package name | Uses `googleNotification.PackageName` from the push body as is in the API call | Rejected unless equal to the configured package (`google/parser.go:156`) |
| Dedupe | None. `messageId` is decoded (`googleStoreNotificationMessage`) and never used; `notificationUUID` is decoded and never used | By `notificationUUID` / `messageId` in the same transaction as the state update |
| Ordering | None. `upsertSubscription` overwrites `expire_time` and `product_id` unconditionally (`core_subscription.go:739-744`); `signedDate` and `eventTimeMillis` unused | `signedDate` / `eventTimeMillis`, stale events recorded and not applied |
| Refund | `refund_time = coalesce(nullif(new,'epoch'), old)`: set once, never cleared, so no REFUND_REVERSED. Google voided sets `refundTime: time.Now()` (`:1523`), not the voided time | Terminal `refunded` with explicit `Restores` path for REFUND_REVERSED; refund of an older Apple transaction does not end a newer one |
| Upgrade / link | Google: row key is `LinkedPurchaseToken` of the new token (one hop). Apple: product overwritten on `UPGRADE` and `DOWNGRADE` (`:890`) | Supersede closes the old chain; Apple product only changes with the transaction |
| Google state source | `subscriptionsv2.get` is called, but only 4 of 13+ RTDN types reach the upsert; `LineItems[0]` is used as the line item | State always read from `subscriptionsv2.get`; line item with the latest expiry |
| Sandbox | Environment stored; `Active()` ignores it (`:703`), so a sandbox row grants access | Ignored by default (`WithAllowSandbox`) |
| Raw payload | `raw_response` and `raw_notification` jsonb columns | `processed_events.raw`, with `Prune` |
| Hooks | `RegisterSubscriptionNotificationApple/Google` callbacks: Subscribed, Renewed, Expired, Cancelled, Refunded | None; callers poll `IsActive` |
| Other stores | Huawei, Facebook Instant, Samsung (#2520); Amazon open (#2496); "custom stores" promised (#2408) | Apple, Google |
| Non-subscription purchases | `purchase` table with refund tracking | Out of scope |
| Retry policy | Parse and signature failures answer 500 (Apple retries a bad body); Apple callback errors answer 200 (event lost); Google purchase callback errors answer 500 | 400 / 401 / 200 / 500 by whether a retry can help |

## 2. Findings

Verdict is for this library. Nakama paths are at the commit above; ours are in this repository.

### 2.1 Google voided-subscription RTDN panics, one refund becomes a week of retries
- URL: [#2530](https://github.com/heroiclabs/nakama/issues/2530), PR [#2531](https://github.com/heroiclabs/nakama/pull/2531) (open), fix landed in commit `eb0d23e9` (PR [#2536](https://github.com/heroiclabs/nakama/pull/2536)), 2026-09-05.
- Problem: the `voidedPurchaseNotification` branch read `googleNotification.SubscriptionNotification.PurchaseToken`, which is nil in a voided message. The panic happens before any response, so Pub/Sub retries for the whole retention (7 days). Reporter measured about 1.3 panics per second from one message, and the refund was never recorded.
- Payload: `{"version":"1.0","packageName":"...","eventTimeMillis":"...","voidedPurchaseNotification":{"purchaseToken":"...","orderId":"...","productType":1,"refundType":1}}`. A license-tester purchase that is cancelled also triggers it.
- Their fix: use `VoidedPurchaseNotification.PurchaseToken`. The same mix-up had been fixed twice before in sibling branches: [#2421](https://github.com/heroiclabs/nakama/pull/2421) (`a002c4c8`, the `GetPurchaseV2Google` argument), [#2432](https://github.com/heroiclabs/nakama/pull/2432) (`6db7f013`, one-time `transactionId`), [#2462](https://github.com/heroiclabs/nakama/pull/2462) (`0abcd981`, the `GetSubscriptionV2Google` argument). The reporter's other suggestion: a recover wrapper that acknowledges, and a Pub/Sub dead-letter policy.
- Ours: **handled** for the mapping. `fromVoided` (`google/parser.go:202-226`) uses its own struct, confirms the void against `latestSuccessfulOrderId`, ignores one-time products and quantity-based partial refunds; tests `google/gaps_test.go` `TestGap4_*`. **Not handled** for the panic class: see 3.2.

### 2.2 Google RTDN types other than purchased / renewed / expired / canceled never update state
- URL: [#2501](https://github.com/heroiclabs/nakama/issues/2501) (open), code `core_subscription.go:1368-1415` (the `switch` is at 1401-1413).
- Problem: the upsert is placed after a `switch` on `notificationType` whose `default` returns 200. PAUSED, RESTARTED, ON_HOLD, IN_GRACE_PERIOD, RECOVERED, REVOKED, DEFERRED, PENDING_PURCHASE_CANCELED are fetched and then dropped, so the stored expiry is stale and the callback never fires.
- Ours: **handled**. The notification type is not used for state; `mapState` (`google/parser.go:366-383`) maps every `subscriptionState`. Unknown states return `ErrUnsupported` with HTTP 200.

### 2.3 Apple subscription REFUND is not persisted (analysis)
- Code: `core_subscription.go:1091-1167`. The `ExpiresDate != 0` branch builds `storageSubscription` and `ValidatedSubscription`, computes `active`, then calls `subscriptionNotificationCallback` without calling `upsertSubscription` and without a nil check on the callback. Before `0451b6ab` ([#2387](https://github.com/heroiclabs/nakama/pull/2387), 2025-09-15) the same branch did call `upsertSubscription` (visible in `0451b6ab^`). No issue was filed.
- Effect: the stored row keeps `refund_time` at epoch, so `Active()` stays true until expiry unless the app's callback writes the refund itself; with no callback registered the handler panics.
- Ours: **handled**. REFUND maps to `refunded` and goes through the same `Apply` as every other event; `apple_domain_test.go:58,92` run REFUND through `Service` and assert state.

### 2.4 Apple case label `"REVOKED"` never matches (analysis)
- Code: `core_subscription.go:971`. Apple's type is `REVOKE` (see `NotificationTypeV2.REVOKE` in Apple's python library, and `testdata/apple/revoke.json` here). Family Sharing revocations fall into `default:` and are dropped.
- Ours: **handled**, `apple/mapper.go:190`.

### 2.5 DOWNGRADE changes the product immediately
- URL: [#2400](https://github.com/heroiclabs/nakama/pull/2400) (`2b42ba31`), code `core_subscription.go:890-897`.
- Problem: for `DID_CHANGE_RENEWAL_PREF` with subtype `UPGRADE` or `DOWNGRADE` the product is taken from `renewalInfo.autoRenewProductId`. An upgrade takes effect at once (the transaction already carries the new product), but a downgrade takes effect at the next renewal, so the user is shown the lower plan for the rest of a period they paid for.
- Payload: `DID_CHANGE_RENEWAL_PREF` / `DOWNGRADE`, `signedTransactionInfo.productId = "pro"`, `signedRenewalInfo.autoRenewProductId = "basic"`.
- Ours: **handled**. Reproduced: `ToEvent` returns `ProductID = "pro"`, state active, `NoRevive` true. The product changes on the next DID_RENEW transaction. There is no test that pins DOWNGRADE; add one (4.1, test 1).

### 2.6 Nested decoding assumes `data` and `signedTransactionInfo` are present (analysis)
- Code: `core_subscription.go:1256` dereferences `notificationPayload.Data` without a nil check; the REFUND branch dereferences `transactionInfo` (`:1092`) with no nil check. SUMMARY, EXTERNAL_PURCHASE_TOKEN and RESCIND_CONSENT notifications carry no `data`; `net/http` recovers the panic and Apple retries.
- Ours: **handled**. `VerifyNotification` returns early when `data` is nil, `ToEvent` returns an error for a missing transaction instead of dereferencing (`apple/mapper.go:80-82`); `apple/priorart_test.go` `TestPriorArt_DatalessAndUnmappedNotifications`.

### 2.7 Apple notifications are accepted from any app and any environment (analysis)
- Code: `iap/iap.go:922-1016`, `core_subscription.go:1234-1300`. Chain to Apple Root CA G3 and the signature are checked; nothing else. No marker OIDs, so any ECDSA leaf issued under that root passes (Apple PKI issues other leaves beneath the same root; whether a third party can obtain an ECDSA P-256 one was not tested here). No bundle ID or environment comparison, so a notification or client-submitted JWS from a different app, or a free Sandbox transaction, is stored; sandbox rows are `Active()` like production ones.
- Related issue: [#2529](https://github.com/heroiclabs/nakama/issues/2529) (a legacy shared secret was required even for JWS) shows the JWS path was added late (`43105465`, 2026-02-12, [#2454](https://github.com/heroiclabs/nakama/pull/2454)).
- Ours: **handled**. This is covered by `DEFINITION_OF_DONE.md` ("Apple — signed notification verification") and `apple/priorart_test.go` `TestPriorArt_WrongMarkerOID`.

### 2.8 Google line item selection and empty `lineItems`
- Code: `core_subscription.go:507`, `:549`, `:1396-1398`, `:1522-1525` index `LineItems[0]`. [#2400](https://github.com/heroiclabs/nakama/pull/2400) removed the comment "multiple entries if upgraded/downgraded, skip" without changing the indexing. A deferred replacement returns two items (the pending one has no `expiryTime`), an empty list panics with index out of range.
- Ours: **handled**. `applyLineItems` (`google/parser.go:332-352`) takes the item with the latest expiry and tolerates an empty list. Reproduced: `EXPIRED` with no line items maps to `expired` without a panic; `ACTIVE` with none returns `ErrNoExpiry`.

### 2.9 Google link chain keyed by the previous token only (analysis)
- Code: `core_subscription.go:1364-1367`, `:513`. The row key of a new token is `LinkedPurchaseToken` (the immediate predecessor). For a chain A, B (linked A), C (linked B): B's row is keyed `A`, so C looks up key `B`, finds nothing, and either inserts a second row or, without an account id, is dropped with "No userId found" and HTTP 200. Related: [#2329](https://github.com/heroiclabs/nakama/pull/2329) (`e57f5af8`) added the lookup by linked token for the user, same one-hop assumption.
- Ours: **partially**. The models differ: we keep one row per token and retire the old one (`service.go:115-120`). Reproduced: events B (linked A) then C (linked B) leave A expired, B expired, C active; events delivered C then B with real times leave B stale, A expired. If B's event is never seen and only C arrives, A (previously active) stays active: `service.go` retires only `ev.SupersedesTxID`, one hop. See 3.5.

### 2.10 A receipt submitted for a different user
- URL: `3f1fc4f2` ([#982](https://github.com/heroiclabs/nakama/pull/982)): `upsertSubscription` now returns `FailedPrecondition "Invalid receipt for userID"` when the stored row belongs to another user; `upsertPurchases` the same. The `ON CONFLICT` clause never updates `user_id`.
- Ours: **partially**. `statemachine.go:101-103 (as of 2026-10-15, working tree)` overwrites `UserID` with any non-empty value on every event. Reproduced: event for chain `o1` with user A, then a newer event for `o1` with user B leaves the chain owned by B (`ListByUser(A)` empty, `ListByUser(B)` one row). The ids come from signed Apple data and from the Play purchase record, so a legitimate flow does not change them; the check is defence in depth. See 3.1.

### 2.11 Notification arrives before the user is known
- Code: `core_subscription.go:858-868`. A SUBSCRIBED / DID_RENEW without `appAccountToken` looks up the validated subscription by `originalTransactionId`; if none exists the handler answers 500 so Apple retries until the client has validated its receipt. Bugs around this: [#2342](https://github.com/heroiclabs/nakama/pull/2342) and [#2350](https://github.com/heroiclabs/nakama/pull/2350) (`uuid.Must(uuid.FromString(""))` panics for the subscription of a deleted account), [#1158](https://github.com/heroiclabs/nakama/pull/1158) (refund of a deleted user), FK-violation skip (`ErrSkipNotification`, `f2026b73`).
- Ours: **partially**. The event is stored keyed by transaction with an empty `UserID` and a warning (`service.go:98-100`), later events keep a known user (`statemachine.go:67`). There is no way to attach a user to an existing chain except by synthesising an event, and no erasure by user. See 3.3 and 3.6.

### 2.12 Google voided time and the missed-RTDN backstop
- URL: `fa2b7259` ([#959](https://github.com/heroiclabs/nakama/pull/959)) added `google_refund_scheduler.go`, which polled `purchases.voidedpurchases.list` every `refund_check_period_min`; fixes [#982](https://github.com/heroiclabs/nakama/pull/982) (`3f1fc4f2`), [#1158](https://github.com/heroiclabs/nakama/pull/1158) (`01547306`). It was deleted in `0451b6ab` ([#2387](https://github.com/heroiclabs/nakama/pull/2387)) in favour of RTDN only. The RTDN path then stamps `refundTime: time.Now()` (`core_subscription.go:1523`) instead of the voided time the poller used (`voidedTimeMillis`).
- Ours: **handled** for the timestamp (`OccurredAt` is `eventTimeMillis`); **not handled** as a backstop: nothing recovers a missed push (endpoint down for longer than Pub/Sub retention, 7 days by default, or Apple's retry window). See idea 4.3.

### 2.13 Items that do not apply or are covered elsewhere

| Item | Source | Verdict |
|---|---|---|
| Apple legacy `verifyReceipt`: subscription vs purchase receipt detection, `latest_receipt` is not a subscription marker | [#971](https://github.com/heroiclabs/nakama/issues/971), [#907](https://github.com/heroiclabs/nakama/issues/907), `6b02d963`, `e873a69d` | N/A, no receipt validation. We read `type` and `expiresDate` of the transaction |
| One receipt with duplicate transaction ids: `ON CONFLICT DO UPDATE command cannot affect row a second time` | [#1194](https://github.com/heroiclabs/nakama/issues/1194), `621c697e` | N/A, `Apply` writes one row per call. Keep in mind if a batch API is ever added (test 11) |
| SQL placeholder bugs in upserts | `1ec3b8bf` (`$8` instead of `$7`, expiry never updated), `d225d207` (missing comma) | N/A. Our stores are tested through `storetest.Run`, which asserts the stored values |
| Google v1 sandbox flag unknown, `purchaseType` omitted in production | [#778](https://github.com/heroiclabs/nakama/issues/778), `f075f747` | Handled differently: `testPurchase` on `subscriptionsv2` sets `EnvironmentGoogleTest` (`google/parser.go:322`) |
| Pending / cancelled one-time purchase accepted | `22b20d3f` ([#1336](https://github.com/heroiclabs/nakama/pull/1336)) | Subscription analogue handled: `PENDING` is `ErrIgnored` (`google/parser.go:378`) |
| Vague "non-200 response from Google service" | [#666](https://github.com/heroiclabs/nakama/issues/666) | Handled: fetch errors are classified into not-found, invalid, config and transient |
| Google OAuth token cache: shared token across credential overrides, lock released after early return | `f4023add`, `3eed4abd` | N/A, the Play SDK owns tokens |
| OAuth `access_token` sent in the URL query (`iap/iap.go:455,573,633`), so it can appear in transport errors | analysis | Not applicable; we also redact the purchase token from errors (`RedactToken`) |
| Acknowledgement of purchases | [#598](https://github.com/heroiclabs/nakama/issues/598) (open) | Same status as `README.md` item 13: app's responsibility |
| `persist=false` ignored on Apple JWS path | `9789d223` ([#2519](https://github.com/heroiclabs/nakama/pull/2519)) | N/A |
| Callback error handling (Apple callback error answers 200 and the event is gone, Google purchase callback answers 500) | `core_subscription.go:1163,1568` | N/A, no callbacks; see idea 4.5 |
| Shared password required for JWS | [#2529](https://github.com/heroiclabs/nakama/issues/2529) | N/A |
| StoreKit 2 requests | [#1333](https://github.com/heroiclabs/nakama/issues/1333), [#2430](https://github.com/heroiclabs/nakama/issues/2430), [#2452](https://github.com/heroiclabs/nakama/pull/2452) | Feature history only (shipped in 3.37.0). Reviewer comment in #2430 repeats the points we already follow: verify the chain to the root, and treat the Server API as truth for live state |
| Other stores and custom stores | [#2408](https://github.com/heroiclabs/nakama/issues/2408), [#1188](https://github.com/heroiclabs/nakama/issues/1188), [#732](https://github.com/heroiclabs/nakama/issues/732), [#2496](https://github.com/heroiclabs/nakama/pull/2496), [#2520](https://github.com/heroiclabs/nakama/pull/2520) | Not in scope; see idea 4.7 |

## 3. Not handled or partially handled, ranked

Severity is for a deployment that exposes both webhooks.

### 3.1 Chain ownership can be reassigned by a later event (medium)
`statemachine.go:101-103 (as of 2026-10-15, working tree)` accepts any non-empty `ev.UserID`. Nakama refuses a mismatching owner (2.10).

Test (root package, `Service` + memory store):
- Input: event 1 `{ID:"1", Source:apple, OriginalTxID:"o1", UserID:"A", State:active, ExpiresAt:+24h, OccurredAt:t0}`, then event 2 `{ID:"2", ... UserID:"B", OccurredAt:t0+1m}`.
- Today (reproduced): `Process` returns entitlement with `UserID == "B"`; `ListByUser("A")` is empty.
- Expected: the chain stays with A. Event 2's state is still applied; `Process` logs a warning and returns an error wrapping a new `ErrOwnerMismatch` so the handler answers 200 and an operator sees it, or the event is applied without the user change. Pick one and pin it; the second keeps Apple/Google delivery simple.
- Control: event 2 with `UserID:""` keeps A; first event with `UserID:""` followed by `UserID:"A"` fills A (existing behaviour, `statemachine_test.go`).

### 3.2 A panic in the parser, `Processor` or `Store` kills the connection (medium to low)
Neither handler recovers (`httpx/apple.go`, `httpx/google.go`, `httpx/httpx.go:122`). `net/http` recovers per connection and closes it without a status, Pub/Sub treats it as a failed push and redelivers with backoff for the retention period: the #2530 failure mode. Our own parsing code has no known panic path (fuzzed), but `Processor` and `Store` are caller-supplied.

Test (`httpx`, handler built with a `Processor` whose `Process` panics, or a `GoogleParser` that does):
- Today (reproduced): `http.Post` returns `EOF`, the server logs a stack trace.
- Expected: HTTP 500, an error log containing the event id and the panic value, and the process keeps serving. Add a note to the README: set a dead-letter topic and `max-delivery-attempts` on the push subscription, because a deterministic panic is a poison message.

### 3.3 No way to attach a user to an unattributed chain (low to medium)
Nakama binds a receipt to the authenticated session user (`ValidateSubscription*`) and has the 500-and-retry race handling (2.11). Here an Apple purchase without `appAccountToken`, or a Google purchase without account ids, can only be attributed by synthesising a full event (`ID`, `State`, `OccurredAt >= UpdatedAt`, `ExpiresAt` copied from the stored row). `ListByUser` and `IsActive(user, product)` never see it before that.

Proposed test, assuming an attach method (name open):
- Input: `Process` SUBSCRIBED for `o1` with no user; `svc.Attach(ctx, SourceApple, "o1", "A")`; `IsActive("A", product)`.
- Expected: true; `Version` incremented; no change to `State` or `ExpiresAt`. A second `Attach` with user `B` returns `ErrOwnerMismatch` and changes nothing. Attach of an unknown chain returns `ErrNotFound`.

### 3.4 Apple notification that carries only renewal info is rejected with 400 (low, unverified)
`apple/mapper.go:80-82` returns a plain error for any mapped type without `signedTransactionInfo`, so the handler answers 400. Nakama's comment on its EXPIRED / GRACE_PERIOD_EXPIRED / DID_CHANGE_RENEWAL_STATUS branch (`core_subscription.go:972-974`) says these "should all contain RenewalInfo but may not contain TransactionInfo". I found no real payload or Apple statement to confirm that; real fixtures in `testdata/apple` carry both. `JWSRenewalInfoDecodedPayload` has `OriginalTransactionID`, `ProductID`, `Environment`, `GracePeriodExpiresDate`, so a fallback is possible.

Test (`apple`, hand-built `Notification` with `Transaction == nil`):
- EXPIRED with renewal info `{OriginalTransactionID:"o1", ProductID:"pro", Environment:"Production"}`: today `apple: EXPIRED without signedTransactionInfo` (reproduced, HTTP 400). Expected: Event `{State: expired, OriginalTxID: "o1", ProductID: "pro"}` with `ExpiresAt` zero.
- DID_FAIL_TO_RENEW / GRACE_PERIOD with renewal info `gracePeriodExpiresDate = T`: expected Event `{State: grace, ExpiresAt: T}`.
- SUBSCRIBED or DID_RENEW without transaction info: keep rejecting (nothing to grant).

### 3.5 Multi-hop `linkedPurchaseToken` leaves grand-parents active when the middle token was never seen (low)
Reproduced with a stub fetcher: A active (expiry +30d), then only C (linked B) arrives; result A active, B expired (tombstone), C active. A also gets its own notifications from Google in normal operation, so this only matters if both B's push and A's own end-of-life push are lost or if A was upgraded with proration.

Test (root package with `google.Parser` + stub fetcher): events A (active), C (active, `linkedPurchaseToken` B), fetcher also answers B with `linkedPurchaseToken` A. Today: A active. Expected, if the parser follows the link when the target chain is unknown: A expired. A cheaper expected result is a documented limitation; the cost of following is one extra `subscriptionsv2.get` per upgrade.

### 3.6 No erasure of a user's data (low)
The `Store` interface has no delete; `Prune` removes `processed_events` by time only (`store/postgres/store.go:265`), and `processed_events.raw` keeps full payloads, which include `appAccountToken`. Nakama's deleted-user handling (#1158, #2342, #2350) is the other side of the same problem.

Test (`storetest`): after `Process` of three events for user A, `DeleteByUser("A")` (name open) removes the entitlement rows and the raw payloads of those events; `ListByUser("A")` is empty; a later redelivery of one of the events is still `ErrDuplicate` (keep the id, drop the payload) and a later event for the same chain creates an unattributed row.

### 3.7 No state-change hook and no auto-renew signal (info)
Nakama exposes Subscribed / Renewed / Expired / Cancelled / Refunded callbacks ([#2387](https://github.com/heroiclabs/nakama/pull/2387), [#2501](https://github.com/heroiclabs/nakama/issues/2501) asks for more). We have none, and "Cancelled" (Apple `AUTO_RENEW_DISABLED`, Google `CANCELED`) is folded into `active`, so a UI cannot say "ends on 12 Nov, not renewing". Not a bug.

## 4. Design ideas worth taking

1. **Pin the cases that Nakama got wrong as regression tests.** Test 1 and 2 below cost nothing and guard behaviour that currently holds only by construction.
2. **Owner binding** (3.1, 3.3). One rule, used by both `Attach` and `Apply`: the first non-empty user wins, a different one is an explicit error.
3. **Sync from a purchase token.** Nakama's primary flow is the client sending the receipt right after purchase, so access does not wait for a notification (Apple notifications and RTDN can take minutes). For Google this is `google.Parser.ParseToken(ctx, token)`: the existing `fromSubscription` with `ID = "sync:"+token+":"+fetchTimeMillis` and `OccurredAt = fetch time`. For Apple, `Verifier.Verify` is already generic; a transaction JWS maps to an Event with `OccurredAt = signedDate`. Keep Nakama's mistake out: it writes whatever the submitted JWS says over the stored row (`expire_time = $7`), so an old JWS replay shortens the expiry. Our ordering key makes that a stale event.
4. **Reconcile.** A `Reconcile(ctx, source, txID)` helper that fetches the current state and feeds it through `Process` as a synthetic event (same code as idea 3), plus an optional Google `voidedpurchases.list` poller. This is what Nakama had in `google_refund_scheduler.go` before it was removed. Useful after an outage longer than the stores' retry windows.
5. **Observer.** `WithObserver(func(ctx, prev *Entitlement, next Entitlement, ev Event))` called after a successful `Apply`, fixed set of transitions. Call it after commit and make observer errors non-fatal; Nakama's inconsistent answer to callback errors (200 vs 500, 2.13) shows the policy has to be decided up front.
6. **`AutoRenew bool` on `Entitlement`**, from Apple `autoRenewStatus` / `DID_CHANGE_RENEWAL_STATUS` subtype and Google `CANCELED`.
7. **Other stores.** `Source` is a string and the state machine is store-agnostic, so Samsung, Amazon, Huawei or a web payment provider only need a parser that returns `Event`. A short "writing a source" paragraph with the invariants (stable `ID`, store-time `OccurredAt`, `OriginalTxID` as chain key) would be enough; Nakama's open requests (#2408, #1188, #732) show the demand.

## 5. Test list

Fixtures are inline; no new files are needed. "Root" is package `entitlements_test`.

1. **Apple DOWNGRADE keeps the product** (Root or `apple`). `DID_CHANGE_RENEWAL_PREF` / `DOWNGRADE`, transaction `productId=pro`, renewal `autoRenewProductId=basic`. Expect Event `ProductID == "pro"`, state active, `NoRevive` true. Then `DID_RENEW` with transaction `productId=basic`: `ProductID == "basic"`. (Passes today.)
2. **Apple REVOKE and REFUND both reach the store** (Root). SUBSCRIBED, then `REVOKE` on a fresh chain: state `revoked`, `IsActive` false. SUBSCRIBED, then `REFUND` with `revocationDate`: state `refunded`, `IsActive` false, `Version == 2`. (Passes; covers 2.3 and 2.4.)
3. **Google states that Nakama drops** (`google`). One case per `subscriptionState` ON_HOLD, IN_GRACE_PERIOD, PAUSED, ACTIVE-after-pause (RESTARTED), EXPIRED with `notificationType` 5, 6, 10, 7, 13: Event state `billing_retry`, `grace`, `paused`, `active`, `expired`. (Passes.)
4. **Google empty line items** (`google`). `EXPIRED` and no `lineItems`: Event expired, no panic. `ACTIVE` and no `lineItems`: `ErrNoExpiry`. (Passes, reproduced.)
5. **Google voided subscription** (`google`). Payload from 2.1 with `productType 1`, fetcher returns `EXPIRED` and `latestSuccessfulOrderId` equal to the voided `orderId`: Event refunded, `OccurredAt == eventTimeMillis`. (Passes.)
6. **Voided order of a superseded token** (Root). A active, B active (linked A). Void for A's last order: A refunded, B unchanged and `IsActive(user, product)` still true. (Check before relying on it; not run.)
7. **Owner mismatch** (3.1). Fails today.
8. **Handler panic** (3.2). Fails today (connection closed instead of 500).
9. **Attach** (3.3). Needs the method.
10. **Renewal-info-only notifications** (3.4). Fails today.
11. **Duplicate key in one call** (only if a batch API is added): two events with the same `OriginalTxID` in one `ProcessBatch` apply in order, no SQL error (#1194).
12. **Multi-hop link, middle token unseen** (3.5). Fails today if the parser should follow links; otherwise document.
13. **Erase by user** (3.6). Needs the method.

## 6. Re-evaluate when

- nakama ships the "modularised IAP / entitlement system" mentioned in [#2408](https://github.com/heroiclabs/nakama/issues/2408) (registerable stores): read its interface for how it models source-specific state.
- [#2501](https://github.com/heroiclabs/nakama/issues/2501) is fixed: it will show which extra Google/Apple states Heroic Labs considers worth a callback.
- Any new nakama issue that mentions `core_subscription.go` panics: the handler still has no recover wrapper.
