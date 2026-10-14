# Prior art: what other libraries got wrong

Notes on what other Apple and Google subscription libraries got wrong, from their public issue trackers, and which of those cases this library covers. The matching fixtures are in `testdata/prior-art/`.

The "Us:" lines record the state of this library when the research was done; most of the gaps they list have since been fixed (see `DEFINITION_OF_DONE.md`).

Method: `gh` listing of all issues (open and closed) and PR titles of each repo, keyword and title triage, then full
bodies and comments for the ~90 relevant ones. Counts: 21 repos, about 950 issue titles scanned, about 1,700 PR titles
scanned, about 90 issues read in full. Where a verdict is my own analysis and no upstream issue backs it, the entry says
"analysis".

## 1. Libraries

Stars and last push from the GitHub API on 2026-10-14.

| Repo | Lang | Stars | Last push | Scope | Issues (all) |
|---|---|---|---|---|---|
| [apple/app-store-server-library-node](https://github.com/apple/app-store-server-library-node) | TS | 383 | 2026-10-12 | Apple JWS verify + Server API | 85 |
| [apple/app-store-server-library-swift](https://github.com/apple/app-store-server-library-swift) | Swift | 300 | 2026-09-28 | same | 26 |
| [apple/app-store-server-library-java](https://github.com/apple/app-store-server-library-java) | Java | 279 | 2026-10-12 | same | 79 |
| [apple/app-store-server-library-python](https://github.com/apple/app-store-server-library-python) | Python | 266 | 2026-09-28 | same | 60 |
| [getmimo/app-store-server-library-dotnet](https://github.com/getmimo/app-store-server-library-dotnet) (`apple/app-store-server-library-dotnet` does not exist, 404) | C# | 31 | 2026-03-02 | Apple JWS verify; several issues were filed by an AI reviewer comparing it to the Java library | 13 |
| [namecare/app-store-server-library-rust](https://github.com/namecare/app-store-server-library-rust) | Rust | 82 | 2026-10-14 | same | 11 |
| [richzw/appstore](https://github.com/richzw/appstore) | Go | 191 | 2026-09-08 | Apple Server API + V2 notification parse | 14 |
| [awa/go-iap](https://github.com/awa/go-iap) | Go | 1060 | 2026-10-13 | Apple (v1 receipts, V2 notifications, Server API), Google Play (v1, subscriptionsv2, RTDN types), Amazon, Huawei | 98 |
| [izniburak/appstore-notifications-go](https://github.com/izniburak/appstore-notifications-go) | Go | 45 | 2026-10-03 | Apple V2 parse only | 5 |
| [voltrue2/in-app-purchase](https://github.com/voltrue2/in-app-purchase) | JS | 1087 | 2026-01-24 | receipt validation, Apple v1 + Google v1 (license NOASSERTION) | 261 fetched |
| [Wizcorp/node-iap](https://github.com/Wizcorp/node-iap) | JS | 272 | 2024-06-25 | same, unmaintained | 34 |
| [Deishelon/google-play-billing-validator](https://github.com/Deishelon/google-play-billing-validator) | JS | 95 | 2022-12-08 | Google v1 verify | 21 |
| [readdle/app-store-server-api](https://github.com/readdle/app-store-server-api) | PHP | 76 | 2026-09-20 | Apple Server API | 20 |
| [imdhemy/laravel-in-app-purchases](https://github.com/imdhemy/laravel-in-app-purchases) | PHP | 390 | 2026-10-14 | Laravel glue for both stores, events | 117 |
| [imdhemy/google-play-billing](https://github.com/imdhemy/google-play-billing) | PHP | 50 | 2026-06-15 | Google subscriptionsv2, RTDN parse, acknowledge/cancel/defer | 53 |
| [imdhemy/appstore-iap](https://github.com/imdhemy/appstore-iap) | PHP | 56 | 2026-10-01 | Apple v1 + V2 notification JWS | 48 |
| [AlexandrFiner/app-store-library-php](https://github.com/AlexandrFiner/app-store-library-php) | PHP | 3 | 2026-10-09 | Apple Server API | 0 (7 PRs) |
| [vipertecpro/mobile-entitlements](https://github.com/vipertecpro/mobile-entitlements) | PHP | 0 | 2026-10-11 | the only other unified two-store package; no issues, no PRs | 0 |
| [googlesamples/play-billing-samples](https://github.com/googlesamples/play-billing-samples) | Java | 34 | 2026-10-05 | Google sample server (the old names `android/play-billing-samples` and `googlesamples/android-play-billing` return 404); the 3 issues are spam | 3 |
| [RevenueCat/purchases-ios](https://github.com/RevenueCat/purchases-ios), [purchases-android](https://github.com/RevenueCat/purchases-android) | Swift, Kotlin | 3077, 562 | 2026-10-14 | client SDKs only; the server side is closed, so there is no webhook logic to learn from. Not mined. | n/a |

Also seen, not mined: `rickwierenga/app-store-notifications-v2-validator` (43 stars, Python, last push 2024-12),
`alexalok/AppStoreServerNotificationsV2` (24 stars, C#, AGPL), `satorunooshie/asn` (9 stars, Go), `Wizcorp` forks.
No open-source library handles Google RTDN together with subscriptionsv2 state fetching to a normalized state;
`imdhemy/*` is the closest and it stops at event dispatch.

Observation worth keeping: the four Apple libraries are the reference for Apple verification and none of them models
entitlement state. Everything in sections 2 and 3 about state mapping comes from the unified-state problem itself, so
the evidence there is thinner than for verification.

## 2. Edge cases, most dangerous first

Verdict legend: handled / partially / not handled / unknown. Paths are relative to the repository root.

### 1. Bundle ID and environment checks are optional and skip nested payloads
- Evidence: [getmimo/dotnet#27](https://github.com/getmimo/app-store-server-library-dotnet/issues/27) (transactions and renewal info not compared with the verifier's bundle ID and environment: "a production verifier accepts any Apple-signed transaction, including sandbox transactions and transactions from other apps. Sandbox purchases are free"); Apple's own libs make `bundleId` a required constructor argument (python `signed_data_verifier.py:32-46`, `_verify_notification` lines 115-119, nested checks lines 73-76); [swift#34](https://github.com/apple/app-store-server-library-swift/issues/34) and [python#114](https://github.com/apple/app-store-server-library-python/pull/114) ("No bundle id is ok" shows they had to decide this explicitly); [node#227](https://github.com/apple/app-store-server-library-node/issues/227): Apple's maintainer advises handling sandbox and production separately, because notifications grant access.
- Payload: any Apple-signed notification from someone else's app, or a free sandbox purchase, carrying `appAccountToken` = a victim's id.
- Us: **partially**. `apple/verifier.go:209-214` checks `data.bundleId` and `data.environment` only when `WithBundleID` / `WithEnvironment` were passed; `NewVerifier` with no options accepts every app and environment (`verifier.go:78-90`). Nested `signedTransactionInfo` / `signedRenewalInfo` are verified (`verifier.go:215-226`) but their `bundleId` and `environment` are never compared. `Event` has no environment field, so a store holding both sandbox and production events cannot tell them apart.
- Fix direction: require bundle ID (constructor arg or an explicit `WithInsecureAnyBundle`), compare nested bundleId/environment, carry `Environment` on `Event`.

### 2. Google ACTIVE with no `expiryTime` means access forever
- Evidence: [imdhemy/google-play-billing#304](https://github.com/imdhemy/google-play-billing/issues/304): with deferred replacement `subscriptionsv2.get` returns two line items and the newly purchased one has no `expiryTime`. Google's reference does not mark `expiryTime` required either.
- Us: **not handled**. `Entitlement.Active` returns true when `ExpiresAt.IsZero()` (`entitlements.go:65`). `google/parser.go:107-118` leaves `ExpiresAt` zero when no line item has an expiry, and `Apply` (`statemachine.go:47`) only overwrites expiry when non-zero, so a zero event would also keep a stale value. A fixture where every line item lacks `expiryTime` grants access with no end. With only one of two items missing it, the max-expiry pick works (old item wins) but the reported `ProductID` stays the old product until the replacement takes effect.
- Fixture: `testdata/prior-art/google-issue-derived/sub_active_no_expiry_anywhere.json`, `sub_deferred_replacement_two_line_items.json`.

### 3. REFUND_REVERSED is unsupported, so a reversed refund leaves the user locked out
- Evidence: [richzw/appstore#17](https://github.com/richzw/appstore/issues/17) (type missing; added in v1.31.0), go-iap `NotificationTypeV2RefundReversed` typo fix (PR #220); python `NotificationTypeV2` enum contains `REFUND_REVERSED`.
- Us: **not handled**. `apple/mapper.go:103` returns `ErrUnsupported` (HTTP 200, logged). `refunded` is terminal (`statemachine.go:39-44`); only a later `active` event strictly after the refund re-activates, so the user stays refunded until the next `DID_RENEW`, which for a yearly plan is a year.
- Expected: REFUND_REVERSED maps to active with the transaction's `expiresDate`, ordered by `signedDate`.

### 4. Google voided purchase is treated as a subscription refund regardless of `productType`
- Evidence: RTDN reference: `voidedPurchaseNotification.productType` is 1 (subscription) or 2 (one-time); `refundType` 1 full, 2 quantity-based partial ([rtdn-reference](https://developer.android.com/google/play/billing/rtdn-reference)); go-iap PR #254, #276 (added `VoidedPurchaseRefundType`) show the field matters.
- Us: **not handled**. `google/parser.go:72-74` creates an Event with `State=refunded` and `OriginalTxID=purchaseToken` for every voided notification. For productType 2 the store gets a new `refunded` row with empty user and product (`statemachine.go:24-34` creates when `cur == nil`). It is harmless to access but pollutes the table and logs a false refund. A partial refund (refundType 2) also kills the whole chain (analysis, no upstream issue).
- Fixture: `testdata/prior-art/google-play-billing-rtdn/voided-one-time-DERIVED.push.json` (derived by us), `voided-subscription.push.json`.

### 5. Google test purchases are granted in production
- Evidence: `testPurchase: {}` is present in the real response quoted in [imdhemy/google-play-billing#228](https://github.com/imdhemy/google-play-billing/issues/228); [voltrue2#248](https://github.com/voltrue2/in-app-purchase/issues/248) "Cannot tell whether a google store purchase is a test purchase"; [Wizcorp#28](https://github.com/Wizcorp/node-iap/issues/28).
- Us: **not handled**. `google/types.go:104-110` does not parse `testPurchase`, so a license-tester purchase (free) is indistinguishable and entitles the user.
- Fixture: `google-issue-derived/sub_active_obfuscated_account_only.json` contains `testPurchase`.

### 6. Renewal extensions are ignored, so extended users lose access at the old expiry
- Evidence: [getmimo/dotnet#25](https://github.com/getmimo/app-store-server-library-dotnet/issues/25) (RENEWAL_EXTENSION/SUMMARY: summary has no `data`, numeric counts and a string array; fixed upstream by reading bundle/environment from `summary`); python `NotificationTypeV2` has both `RENEWAL_EXTENDED` and `RENEWAL_EXTENSION`.
- Us: **partially**. Summary notifications parse (`apple/types.go:32` keeps `summary` raw, `verifier.go:206-208` tolerates missing `data`), then map to `ErrUnsupported`. `RENEWAL_EXTENDED` (a per-subscriber notification with the new `expiresDate`) is also unsupported (`mapper.go:103`), so the stored `ExpiresAt` stays at the old value.
- Fixture: `app-store-server-library-python/models/signedSummaryNotification.json` (decoded).

### 7. Google profile-id-only accounts get no user
- Evidence: [imdhemy/google-play-billing#228](https://github.com/imdhemy/google-play-billing/issues/228): `externalAccountId` and `obfuscatedExternalProfileId` are optional; apps may set only the profile id (`setObfuscatedProfileId`).
- Us: **not handled**. `google/parser.go:103-105` reads only `obfuscatedExternalAccountId`, `google/types.go:119-121` has no profile field. The event has an empty `UserID`, stored as unattributed (README: acknowledged and logged).
- Fixture: `google-issue-derived/sub_active_profile_id_only.json`.

### 8. Apple REFUND for an older transaction revokes the whole chain
- Evidence: none upstream (analysis). Apple documents REFUND as refunding one transaction, and the `signedTransactionInfo` is that transaction.
- Us: **partially**. `apple/mapper.go:96-97` and `statemachine.go:39-44` mark the chain `refunded` whatever transaction was refunded and keep it terminal until a later `active` event. If the user refunds last month's charge but this month's is paid, access is cut until the next renewal. Needs a compare of the refunded `transactionId` / `expiresDate` with the chain's current expiry.

### 9. Chain verification uses wall-clock time and has no revocation check
- Evidence: Apple libs validate at the payload `signedDate` when online checks are off (python `signed_data_verifier.py:170-172`; [getmimo/dotnet#26](https://github.com/getmimo/app-store-server-library-dotnet/issues/26)); [java#206](https://github.com/apple/app-store-server-library-java/issues/206) INVALID_CHAIN after old Apple certificates expired on 2025-10-11 (Apple: StoreKit 2 re-fetches transactions, a device concern); OCSP is optional, with real-world pain: [java#14](https://github.com/apple/app-store-server-library-java/issues/14) and [java#117](https://github.com/apple/app-store-server-library-java/issues/117) (China latency / network errors), [python#116](https://github.com/apple/app-store-server-library-python/issues/116), [node#447](https://github.com/apple/app-store-server-library-node/issues/447) (stale OCSP accepted because GeneralizedTime parsed as NaN), plus retryable-failure PRs (java#204, python#163, node#345, swift#104).
- Us: **partially, by design**. `verifier.go:147` uses `timeFn()` (default now), documented at `verifier.go:63-68`. For live webhooks that is correct (Apple signs fresh); it matters for replay of stored payloads. No OCSP at all; accepted risk, should be stated in the README.

### 10. Chain length and root set
- Evidence: Apple libs require exactly 3 certificates (python line 202 `INVALID_CHAIN_LENGTH`). [node#226](https://github.com/apple/app-store-server-library-node/issues/226) and [java#98](https://github.com/apple/app-store-server-library-java/issues/98): worried that AppleComputerRootCertificate expires 2025-02; Apple maintainer: signing is with Apple Root CA - G3 (expires 2039), rotations happen in intermediate and leaf only. [richzw#3](https://github.com/richzw/appstore/issues/3) / [#26](https://github.com/richzw/appstore/issues/26): early Go library verified the chain but not the leaf or the signature itself (fixed later). [rust#60](https://github.com/namecare/app-store-server-library-rust/issues/60): `verify_chain` failed when extra roots with SHA1-RSA were in the pool.
- Us: **handled**. G3 only is embedded (`verifier.go:23-47`); leaf and intermediate OIDs checked (`verifier.go:134-139`); signature verified against the leaf (`verifier.go:153-169`); `alg` pinned to ES256 (`verifier.go:117`). Difference: we accept 2 or more certs (`verifier.go:120`) where Apple requires exactly 3. Low risk because roots are pinned and OIDs checked.
- Checked on 2026-10-14: Apple's own test JWS from the python repo verify with our `Verifier` when the test CA is the root (see `testdata/prior-art/app-store-server-library-python/SOURCE.md`).

### 11. Missing `appAccountToken`
- Evidence: [java#135](https://github.com/apple/app-store-server-library-java/issues/135): Apple's maintainer confirms it is null whenever the device did not send one, and for all pre-existing purchases.
- Us: **partially**. `apple/mapper.go:73` sets `UserID` empty; the state machine keeps an existing `UserID` when a later event has none and fills it when one has (`statemachine.go:53-55`). `ListByUser` (`store/postgres/store.go:225`) cannot find unattributed rows, and there is no resolver hook (stated at `mapper.go:14-18`).

### 12. Multi-line-item Google subscriptions (add-ons, bundles)
- Evidence: Google reference: `lineItems` is an array; [imdhemy#304](https://github.com/imdhemy/google-play-billing/issues/304).
- Us: **partially**. `google/parser.go:107-118` collapses to one product (latest expiry). A second product on the same token is invisible to `IsActive(user, product)`.
- Fixture: `google-issue-derived/sub_multi_line_item_addon.json`.

### 13. Google acknowledgement
- Evidence: [voltrue2#290](https://github.com/voltrue2/in-app-purchase/issues/290), [Deishelon#7](https://github.com/Deishelon/google-play-billing-validator/issues/7), [Deishelon#14](https://github.com/Deishelon/google-play-billing-validator/issues/14), imdhemy PR "support acknowledge subscription purchase" (#193). Unacknowledged purchases are refunded by Google after 3 days (Play Billing integration docs).
- Us: **not handled**. `google/types.go:109` parses `acknowledgementState`, nothing uses it. Fine if the app acknowledges client-side; the README should say who is responsible.

### 14. Play API failures: transient and permanent are both 500
- Evidence: [Deishelon#22](https://github.com/Deishelon/google-play-billing-validator/issues/22) (401 insufficient permissions), [Deishelon#15](https://github.com/Deishelon/google-play-billing-validator/issues/15) (503), [laravel-in-app-purchases#94](https://github.com/imdhemy/laravel-in-app-purchases/issues/94), [swift#70](https://github.com/apple/app-store-server-library-swift/issues/70) (Apple split retryable from permanent verification failures, PRs java#204 / python#163 / node#345 / swift#104).
- Us: **partially**. `google/parser.go:89` wraps every fetch error; `httpx/google.go` answers 500 so Pub/Sub retries. A permanently failing token (permissions, deleted purchase) retries until the subscription's dead-letter or retention limit (analysis).

### 15. Unknown or new notification types
- Evidence: [go-iap#323](https://github.com/awa/go-iap/issues/323) (`SUBSCRIPTION_PRICE_STEP_UP_CONSENT_UPDATED` = 22 caused 400s because the enum was unknown), [go-iap#335](https://github.com/awa/go-iap/issues/335) (`pendingRefundReviewNotification`), [laravel-in-app-purchases#416](https://github.com/imdhemy/laravel-in-app-purchases/issues/416) (ONE_TIME_CHARGE error spam), [node#98](https://github.com/apple/app-store-server-library-node/issues/98), [node#311](https://github.com/apple/app-store-server-library-node/issues/311) (EXTERNAL_PURCHASE_TOKEN), java PR #50 (accept unknown enum values).
- Us: **handled**. `notificationType` is an int used as a hint only (`google/types.go:73`); unknown RTDN kinds fall through to `ErrUnsupported` (`parser.go:77-78`) and answer 200; Apple unknown types are `ErrUnsupported` (`mapper.go:103`), ONE_TIME_CHARGE is `ErrIgnored` (`mapper.go:38-48`). The Apple list as of the python/node libraries: `CONSUMPTION_REQUEST, DID_CHANGE_RENEWAL_PREF, DID_CHANGE_RENEWAL_STATUS, DID_FAIL_TO_RENEW, DID_RENEW, EXPIRED, EXTERNAL_PURCHASE_TOKEN, GRACE_PERIOD_EXPIRED, METADATA_UPDATE, MIGRATION, OFFER_REDEEMED, ONE_TIME_CHARGE, PRICE_CHANGE, PRICE_INCREASE, REFUND, REFUND_DECLINED, REFUND_REVERSED, RENEWAL_EXTENDED, RENEWAL_EXTENSION, RESCIND_CONSENT, REVOKE, SUBSCRIBED, TEST`. Google subscription types: 1-13, 17-20, 22 (14-16, 21 unused).

### 16. Notifications without `data` (RESCIND_CONSENT, EXTERNAL_PURCHASE_TOKEN, SUMMARY)
- Evidence: python `signed_data_verifier.py:90-112` reads bundle id from `data`, `summary`, `externalPurchaseToken` or `appData`; [node#359](https://github.com/apple/app-store-server-library-node/issues/359) (appData payloads not verified); [node#370](https://github.com/apple/app-store-server-library-node/issues/370).
- Us: **handled but unchecked**. `verifier.go:205-208` returns the notification with no bundle or environment check when `data` is nil; all such types map to `ErrUnsupported` so no state changes. If one is ever mapped to state, the bundle check must be added first.

### 17. Malformed input must not panic
- Evidence: [izniburak#5](https://github.com/izniburak/appstore-notifications-go/issues/5) (panic when `signedRenewalInfo` missing), [#4](https://github.com/izniburak/appstore-notifications-go/issues/4) (panic on invalid JSON), [go-iap PR #291](https://github.com/awa/go-iap/pull/291), [getmimo/dotnet#21](https://github.com/getmimo/app-store-server-library-dotnet/issues/21) (bad base64 / non-JSON header throws non-verification exceptions, handler returns 500).
- Us: **handled**. `verifier.go:105-132` returns `ErrInvalidSignature` for bad segments, base64, header JSON and x5c; `signedRenewalInfo` is optional (`verifier.go:221`). Not covered by a test with Apple-shaped garbage; see test list.

### 18. Timestamps and types
- Evidence: [go-iap#248](https://github.com/awa/go-iap/pull/248) changed RTDN `eventTimeMillis` to a number and [#249](https://github.com/awa/go-iap/pull/249) reverted it ("the actual value is a string"); [go-iap#187](https://github.com/awa/go-iap/issues/187) / #256 numeric-string fields in V1; [getmimo/dotnet#14](https://github.com/getmimo/app-store-server-library-dotnet/issues/14) `isUpgraded` is a bool, [#12](https://github.com/getmimo/app-store-server-library-dotnet/issues/12) `notificationUUID` casing; [java#208](https://github.com/apple/app-store-server-library-java/issues/208) China `purchaseDate` later than `signedDate` (Apple: App Store side).
- Us: **handled**. `google/types.go:27-49` accepts string or number; Apple types match (`apple/types.go:37-65`); we use `expiresDate` and `signedDate`, not `purchaseDate`.

### 19. Google RTDN missing fields, linked token null
- Evidence: [imdhemy/google-play-billing#274](https://github.com/imdhemy/google-play-billing/issues/274) and [laravel-in-app-purchases#437](https://github.com/imdhemy/laravel-in-app-purchases/issues/437) (no `subscriptionId` in some events), [#3](https://github.com/imdhemy/google-play-billing/issues/3) / [#4](https://github.com/imdhemy/google-play-billing/issues/4) (`linkedPurchaseToken` absent), [laravel#3](https://github.com/imdhemy/laravel-in-app-purchases/issues/3) (test notification crashed).
- Us: **handled**. `ev.ProductID` falls back to the line item's product (`parser.go:102,115-117`); `LinkedPurchaseToken` has `omitempty` and is only used when non-empty (`service.go:51`); test notifications are `ErrIgnored` (`parser.go:69-70`).

### 20. Upgrade and crossgrade
- Evidence (Google): [imdhemy/google-play-billing#4](https://github.com/imdhemy/google-play-billing/issues/4) (`linkedPurchaseToken` on resubscribe), [laravel-in-app-purchases#104](https://github.com/imdhemy/laravel-in-app-purchases/issues/104) (link receipts and RTDN with purchaseToken and linkedPurchaseToken). Evidence (Apple, v1 era): [voltrue2#342](https://github.com/voltrue2/in-app-purchase/issues/342), [Wizcorp#90](https://github.com/Wizcorp/node-iap/issues/90): upgrade monthly to yearly adds an extra cancelled monthly transaction after the yearly one, and "latest" picked by sort order or transaction id is wrong.
- Us: Google **handled** (`service.go:51-55, 71-93`: supersede closes the old chain, skips terminal and expired chains); Apple **unknown**: keyed by `originalTransactionId` with ordering by `signedDate`, never sorted by transaction id, so the v1 trap does not apply, but no fixture replays an UPGRADE / DOWNGRADE sequence (see test list).

### 21. Google grace period expiry time
- Evidence: none upstream. Analysis.
- Us: **unknown**. In `IN_GRACE_PERIOD` the mapper uses the line item `expiryTime` (`parser.go:107-114`). If Google reports the original (already past) expiry there, `Active(now)` is false while the state is `grace` and the user is locked out during grace. Apple is handled by taking `gracePeriodExpiresDate` (`mapper.go:65-69`). Must be checked against a live grace-period `subscriptionsv2.get` response before release.

### 22. Xcode / LocalTesting signed data
- Evidence: [python#22](https://github.com/apple/app-store-server-library-python/issues/22), [python#112](https://github.com/apple/app-store-server-library-python/issues/112), java PR #49: Xcode StoreKit-testing JWS has a 1-certificate x5c and `environment: Xcode`.
- Us: **not handled, intentional**. `verifier.go:120` rejects x5c with fewer than 2 certs. The demo `examples/sign-fixture` covers local testing. Worth one sentence in the README.

### Smaller notes
- Apple production endpoint rejects test requests before an app release ([python#51](https://github.com/apple/app-store-server-library-python/issues/51)): not our concern, no API client.
- Server API JWT `exp` must be under 60 minutes ([richzw#28](https://github.com/richzw/appstore/issues/28), go-iap PR #285): not applicable, no client.
- Large payloads: go-iap#240 was a v1 receipt problem; our body cap is 1 MiB (README).
- Pub/Sub push authentication is requested but missing in `imdhemy/google-play-billing#183` and `laravel-in-app-purchases#463`; we already have `httpx.NewPubSubAuthenticator`.
- Android `eventTimeMillis` ordering: since the state is fetched at processing time, a late older event is dropped as stale but its fetched state is not older than the newer event's (analysis; no issue).

## 3. Top 10 gaps in go-entitlements

1. Bundle ID / environment optional, nested payloads unchecked (section 2, item 1).
2. Google ACTIVE with zero `expiryTime` grants unlimited access (item 2).
3. REFUND_REVERSED unsupported (item 3).
4. Voided one-time and partial refunds create or kill entitlements (item 4).
5. `testPurchase` not parsed (item 5).
6. RENEWAL_EXTENDED / extension ignored (item 6).
7. Profile-id-only Google accounts unattributed (item 7).
8. Apple REFUND of an older transaction revokes the chain (item 8).
9. Google grace-period expiry semantics unverified (item 21).
10. Permanent Play API errors retried as 500 (item 14); acknowledgement ownership undocumented (item 13).

## 4. Test cases to add

Fixture paths are relative to `testdata/prior-art/`. "Event" means the `entitlements.Event` returned by the parser, "state" means the `Entitlement` after `Service.Process`.

### Apple
1. **Apple library JWS compatibility.** Input: `app-store-server-library-python/mock_signed_data/{testNotification,transactionInfo,renewalInfo}` with root pool = `certs/testCA.der` and `WithTimeFn(2023-04-12)`. Expect: `Verify` returns nil. (Already checked manually on 2026-10-14; make it a test.)
2. **Missing x5c.** Input: `mock_signed_data/missingX5CHeaderClaim`. Expect: `ErrInvalidSignature` containing `x5c must contain`.
3. **Legacy receipt body.** Input: `mock_signed_data/legacyTransaction`. Expect: `ErrInvalidSignature` (`want 3 segments`), no panic.
4. **Wrong marker OID.** Sign a JWS with `app-store-server-library-java/certs/testInvalidOIDLeaf.pem` and again with `testInvalidOIDIntermediate.pem` chained to `testCA.pem`. Expect: `ErrInvalidSignature` naming the missing OID. Control: `testLeaf.pem` + `testIntermediate.pem` verifies.
5. **Nested bundle mismatch.** Outer notification with `bundleId=com.example`, nested transaction with `bundleId=com.other`, verifier pinned to `com.example`. Expect (after fix): `ErrInvalidSignature`. Today: accepted.
6. **No bundle pinned.** `NewVerifier()` with no options and a foreign `bundleId`. Expect (after fix): constructor error or explicit opt-in.
7. **Sandbox in production.** `WithEnvironment("Production")`, notification `environment: Sandbox`. Expect `ErrInvalidSignature`. Also: nested transaction `environment: Sandbox` inside a Production outer.
8. **REFUND_REVERSED.** Sequence SUBSCRIBED (t1), REFUND (t2), REFUND_REVERSED (t3, same transaction). Expect: events state active, refunded, active; final `Active(now)` true until `expiresDate`.
9. **RENEWAL_EXTENDED.** Active chain expiring T, then RENEWAL_EXTENDED with `expiresDate` T+30d. Expect: `ExpiresAt` = T+30d, state active.
10. **Summary and appData notifications.** Inputs `app-store-server-library-python/models/signedSummaryNotification.json`, `signedRescindConsentNotification.json`, `signedExternalPurchaseTokenNotification.json` (re-signed with the test CA, as `examples/sign-fixture` does; the shipped files hold decoded payloads with placeholder nested strings). Expect: verify succeeds, `ToEvent` returns `ErrUnsupported`, HTTP 200, no row written.
11. **CONSUMPTION_REQUEST and REFUND_DECLINED.** Input `signedConsumptionRequestNotification.json` re-signed. Expect `ErrUnsupported`, HTTP 200.
12. **Missing renewal info on grace.** DID_FAIL_TO_RENEW/GRACE_PERIOD without `signedRenewalInfo`. Expect today: state grace, `ExpiresAt` = transaction expiry (already past, `Active` false). Decide intended behavior and pin it.
13. **Refund of an older transaction.** Chain renewed twice; REFUND carries the first transaction (`revocationDate` set, `expiresDate` earlier than the chain's current expiry). Expect (target): state stays active until the current expiry. Today: refunded.
14. **Upgrade sequence.** DID_CHANGE_RENEWAL_PREF/UPGRADE with a new `productId` under the same `originalTransactionId`, then DID_RENEW. Expect: one entitlement, `ProductID` = new product, no second row.
15. **Equal `signedDate`.** Two events with the same millisecond (DID_FAIL_TO_RENEW then DID_RENEW). Expect: pin current behavior (both applied, last delivered wins) and document it.
16. **Garbage.** Valid base64 header with `x5c` of 3 random strings; a JWS where `signedRenewalInfo` is the empty string; truncated JSON body. Expect: errors, no panic (cf. izniburak#4, #5).
17. **Missing `appAccountToken`.** SUBSCRIBED without token, then DID_RENEW with token. Expect: row `UserID` empty after the first event, filled after the second, `ListByUser` finds it only then.

### Google
18. **Zero expiry.** Input `google-issue-derived/sub_active_no_expiry_anywhere.json`. Expect (target): `Event` rejected or `ExpiresAt` set to a safe value; never `Active` forever. Today: `Active(now)` true with no end.
19. **Deferred replacement.** Input `sub_deferred_replacement_two_line_items.json`. Expect: `ExpiresAt` = 2025-11-01T10:00:00Z, `ProductID` = `pro.monthly`.
20. **Profile id only.** Input `sub_active_profile_id_only.json`. Expect (target): `UserID` = `profile-1234` or a documented option; today empty.
21. **Account id only with `testPurchase`.** Input `sub_active_obfuscated_account_only.json`. Expect (target): configurable rejection of test purchases; today active.
22. **Multi line item.** Input `sub_multi_line_item_addon.json`. Expect: today `ProductID` = `addon.storage`, `ExpiresAt` 2030-01-02; target: both products visible through `IsActive`.
23. **Voided one-time product.** Input `google-play-billing-rtdn/voided-one-time-DERIVED.push.json`. Expect (target): `ErrIgnored`, no row. Control: `voided-subscription.push.json` yields refunded.
24. **One-time product notification.** Input `google-play-billing-rtdn/one-time-product.push.json`. Expect `ErrUnsupported`, HTTP 200, fetcher never called.
25. **RTDN without `subscriptionId`.** Input `subscription-no-subscriptionId.push.json` with the fetcher returning a line item `productId=pro.monthly`. Expect: `Event.ProductID == "pro.monthly"`.
26. **New notification types.** Body with `subscriptionNotification.notificationType` 22, and a body with `pendingRefundReviewNotification`. Expect: 22 fetches and maps by state; the pending-refund body is `ErrUnsupported`, HTTP 200.
27. **`eventTimeMillis` as number and as string.** Both parse to the same `OccurredAt`.
28. **Grace period.** Live-captured (or documented) `IN_GRACE_PERIOD` response; assert `Active(now)` is true when `now` is between the missed renewal time and the grace end. Today this depends on what Google puts in `expiryTime`; capture a real one.
29. **Linked token edge cases.** (a) new token with `linkedPurchaseToken` whose old chain is already expired: no extra write; (b) old chain refunded: stays refunded; (c) delayed old-token EXPIRED event after the supersede: stale or no-op; (d) linked token equals own token: no supersede.
30. **Fetcher errors.** Fetcher returns 403 / 404 / 410 / 503. Expect today: 500 for all; decide which become 200 plus log.
31. **Out-of-order Pub/Sub.** Event B (t2, state from fetch) processed before event A (t1). Expect: A is `ErrStale`; final state equals B's.
