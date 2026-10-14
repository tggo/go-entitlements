# Definition of Done — living spec of go-entitlements

A **point-in-time snapshot** of how this project is supposed to behave *right now*.
It is the source of truth for "is it working correctly?" — not a changelog.

**Rule:** every change that adds or alters observable behaviour MUST add or update
its entry here, **in the same commit** as the code. One entry per feature:

- **What** — one line: the feature / behaviour.
- **DoD** — observable acceptance criteria (what a human or QA can check), not
  implementation detail.
- **Verified** — how it was confirmed (a real measurement) + date, or `—`.

Keep entries terse. When behaviour changes, **edit the existing entry** — never
append a second one. Group by area; new areas go at the bottom.

**Never invent a DoD.** If the acceptance criteria are not clear, ASK and record
the answer — a guessed criterion is worse than none.

---

## Apple — signed notification verification
**What:** `apple.Parser` accepts only App Store Server Notifications V2 whose JWS chains to Apple Root CA G3 and belongs to the configured bundle ID.
**DoD:**
- a real Apple-signed Sandbox or Production notification for the configured bundle parses; one changed signature byte → `ErrInvalidSignature` (HTTP 401)
- `NewVerifier("")` is an error unless `WithInsecureAnyBundle()` is passed
- bundle ID and environment are also checked on the nested transaction and renewal JWS; a Sandbox transaction inside a Production notification is rejected
- a chain to another root, a missing Apple marker OID, an expired leaf or a missing `x5c` is rejected
**Verified:** 5 real fridge.menu notifications (4 Sandbox, 1 Production) through `examples/server`, tampered copy → 401, 2026-10-14; Apple library test JWS with their test CA, 2026-10-14.

## Apple — notification mapping
**What:** each `notificationType`/`subtype` maps to one normalized state or is explicitly ignored; ambiguous types read the state Apple reports.
**DoD:**
- SUBSCRIBED, DID_RENEW → active; DID_FAIL_TO_RENEW+GRACE_PERIOD → grace until `gracePeriodExpiresDate`; DID_FAIL_TO_RENEW, GRACE_PERIOD_EXPIRED → billing_retry; EXPIRED → expired; REFUND → refunded; REVOKE → revoked; REFUND_REVERSED restores the chain
- DID_CHANGE_RENEWAL_STATUS, DID_CHANGE_RENEWAL_PREF, OFFER_REDEEMED, RENEWAL_EXTENDED take the state from `data.status` (1 active, 2 expired, 3 billing retry, 4 grace, 5 revoked), else from `expiresDate` vs `signedDate`; a transaction with `revocationDate` is refunded whatever the type
- only SUBSCRIBED, DID_RENEW and REFUND_REVERSED can lift a refunded or revoked chain: AUTO_RENEW_DISABLED after a refund leaves it refunded; a grace user hit by a plan or renewal-status change keeps access until the grace end
- an active or grace event without an expiry date → `ErrNoExpiry` (200 + error log), never open-ended access
- TEST, ONE_TIME_CHARGE, RENEWAL_EXTENSION, REFUND_DECLINED, PRICE_INCREASE, PRICE_CHANGE, CONSUMPTION_REQUEST, METADATA_UPDATE, MIGRATION, EXTERNAL_PURCHASE_TOKEN, RESCIND_CONSENT, CHILD_CONSENT_REVOKED → ignored with HTTP 200; REFUND/REVOKE of a non-subscription transaction → ignored; unknown types → 200 and a log line
- a missing `appAccountToken` yields an event with empty UserID, keyed by `originalTransactionId`, applied with a warning; `ListByUser("")` returns nothing
- `autoRenewStatus` (renewal info), or the AUTO_RENEW_ENABLED/DISABLED subtype, sets `Event.AutoRenew`; absent means unknown and never overwrites the last known value
- a refund names one transaction (`transactionId`) and is recorded in `Entitlement.RefundedTransactions`: it ends the chain only if that transaction is the one granting access; a refund of an older transaction leaves a running chain alone, and a renewal (a different, unrefunded transaction that outlasts the refunded one) delivered after the refund still applies even if it was signed earlier; a late event for a refunded transaction is stale; REFUND_REVERSED removes it from the list
**Verified:** real SUBSCRIBED → DID_CHANGE_RENEWAL_STATUS → EXPIRED chain from fridge.menu ends `expired`, 2026-10-14 (private fixtures, not in CI); refund/renewal in either arrival order in `refund_order_test.go` and the `storetest` suite, 2026-10-15; domain probes ported as `apple_domain_test.go`, 2026-10-15.

## Core — user attribution and erasure
**What:** every chain can get exactly one owner, and a user's data can be erased.
**DoD:**
- an event without a user ID takes the user of the chain it supersedes, else the `WithUserResolver` answer (an error fails `Process` so the store retries; an empty answer applies it unattributed)
- `Service.LinkUser(source, originalTxID, userID)` attaches a user to a chain without one; repeating it changes nothing; a chain owned by someone else → `ErrUserConflict`, unknown chain → `ErrNotFound`; two concurrent links of different users: exactly one wins
- a chain's owner never changes: a later event with no user or with another user keeps it (the other-user case is logged as a warning) while its state still applies
- `WithOnChange` is called after each committed change (new chain, applied event, retired superseded chain, user link) with before/after; not for duplicates, stale or ignored events; a panic in it is recovered and logged
- `Service.ForgetUser(userID)` deletes that user's entitlements and, in Postgres, nulls the stored raw payloads of their chains; event IDs stay, so a redelivered webhook is still a duplicate; an empty user ID is an error
- `Entitlement.AutoRenew` round-trips through every store; nil means never reported
**Verified:** `service_users_test.go`, the `storetest` suite on memory and Postgres, Postgres `DeleteUser` raw-payload test, 2026-10-15.

## Sandbox and test purchases
**What:** a free test purchase never unlocks production access by default.
**DoD:**
- `Event.Environment` / `Entitlement.Environment` carry Production, Sandbox, Xcode, LocalTesting or GoogleTest (empty = production); `Sandbox()` is true for all but production
- `Service.Process` returns `ErrIgnored` for a non-production event and stores nothing, unless the service is built `WithAllowSandbox()`; `IsActive` skips non-production entitlements unless allowed
- `apple.WithEnvironment` takes `entitlements.EnvironmentProduction` or `EnvironmentSandbox`; anything else is a constructor error
- the Postgres migration turns the old `sandbox` column into `environment`
**Verified:** `service_test.go`, `store/postgres` migration test, 2026-10-15.

## Google — RTDN handling
**What:** `google.Parser` decodes Pub/Sub push RTDN and takes the state from `purchases.subscriptionsv2.get`, not from the notification.
**DoD:**
- subscriptionState ACTIVE/CANCELED(before expiry) → active, IN_GRACE_PERIOD → grace, ON_HOLD → billing_retry, PAUSED → paused, EXPIRED → expired
- an active subscription with no `expiryTime` on any line item is never granted
- `linkedPurchaseToken` retires the old chain; a voided subscription → refunded; a voided one-time product or a partial refund is ignored
- `testPurchase` sets `Event.Environment` to GoogleTest; `WithRejectTestPurchases()` rejects it
- Play API 404/410 → 200 (no retry), 401/403 → 500 with an error log, 5xx/timeout → 500
- a subscription with add-ons yields one event per line item (`purchaseToken#productId`); a single-item purchase keeps the plain token key and `messageId`; items without `expiryTime` grant nothing; a lapsed add-on yields no event
- `linkedPurchaseToken` is followed (at most 4 tokens) so every replaced token and every item of it is retired; a transient failure fetching a replaced token retries the push
- `Event.AutoRenew` comes from each line item's `autoRenewingPlan.autoRenewEnabled` (nil for prepaid or absent plans, never false by default)
- `Parser.ParseToken` syncs from a purchase token with deterministic, token-free event IDs and an `OccurredAt` that cannot beat a newer push
- the HTTP handlers turn a panic in the parser, processor, store or acknowledger into a 500 with a stack in the log
- `httpx.WithAcknowledger` acknowledges pending, access-granting purchases only after the entitlement is stored; transient failures answer 500 and the duplicate redelivery acknowledges again
- importing `google` or `httpx` pulls no `google.golang.org/api`; the SDK lives in `google/androidpublisher` and `httpx/pubsubauth`
**Verified:** fixtures from Google docs and public issues only; no live Play traffic yet. Acknowledge flow: `httpx/ack_test.go`, `google/ack_test.go`, `google/androidpublisher/ack_test.go` (a local fake of the Play API, not the live one).

## Core — ordering, idempotency, storage
**What:** `Service.Process` applies each event at most once and never lets an older event overwrite a newer one.
**DoD:**
- a redelivered event (same source + event ID) → HTTP 200, no state change
- an event older than the stored one → recorded as stale, HTTP 200, no state change
- a supersede arriving before the old chain is known leaves a tombstone, so the old chain's late event is stale
- under concurrent delivery the stored state equals the newest event and `Version` equals the number of applied events
- every `Store` implementation passes `storetest.Run`; memory and Postgres do
- Postgres `Apply` runs at Read Committed and retries SQLSTATE 40001/40P01
- an event with a NUL byte or an identifier over 256 bytes → `ErrInvalidEvent`; `Service` offers `Get` and `ListByUser`
- the README quickstart is compiled (`quickstart_example_test.go`) and a test keeps the README identical to it
**Verified:** `make test-unit` and `make test-integration` with `-race` on Postgres 17; `make stress` heavy (9,600 deliveries, 64 goroutines, crossing supersedes): memory ~40k+/s, Postgres 1,996/s (20 conns), 523/s (2 conns), 1,609/s (Serializable default), no deadlocks; fuzz 30s per target, no crashers, 2026-10-14.

## HTTP — response policy
**What:** `httpx` handlers answer so stores retry only when a retry can help.
**DoD:**
- 200: applied, duplicate, stale, ignored, unsupported, invalid event, purchase not found
- 400 malformed body, 401 bad signature or Pub/Sub token, 413 body over limit (1 MiB default), 405 non-POST
- 500: storage or transient fetch errors
- `pubsubauth.New` refuses an empty audience or service account email
**Verified:** e2e integration test (HTTP → Service → Postgres), 2026-10-14.

## Apple — Server API client (apple/serverapi)
**What:** `serverapi.Client` calls the App Store Server API (statuses, transaction history, transaction info, order lookup, test notification, notification history); every signed value is verified with `apple.Verifier`.
**DoD:**
- each request carries a fresh ES256 JWT (kid, iss, iat, exp at most 60 min, aud appstoreconnect-v1, bid); the key and tokens never appear in logs, errors or fmt output
- a signed value of another bundle ID or environment, or with a bad signature, is refused (`ErrIdentityMismatch` / `ErrInvalidSignature`); nothing partially verified is returned
- 429, 5xx, transport errors and Apple's "try again" codes are retried with backoff; Retry-After is read as UNIX ms (also seconds / HTTP date); a wait over 30 s is returned as `ErrRateLimited`, not slept
- Apple errors are `*APIError`; every 404 matches `entitlements.ErrNotFound`
- paged listings stop on a repeated cursor instead of looping
**Verified:** httptest tests with a generated Apple CA, 2026-10-15; live Sandbox for menu.fridge.recipebox (build tag `live`): TEST notification verified, 4 history records verified, 2026-10-15.

## Apple — Reconcile and history replay
**What:** `Reconcile` repairs a lost webhook from Get All Subscription Statuses; `ReplayNotificationHistory` re-feeds notifications Apple says it sent.
**DoD:**
- state from status 1..5 (grace expiry is the later of expiresDate and gracePeriodExpiresDate; 5 is revoked for Family Sharing, else refunded); an active/grace snapshot without its date is `apple.ErrNoExpiry`, never open-ended access
- OccurredAt is Apple's signedDate, so a snapshot cannot overwrite a newer webhook and an older webhook cannot undo a snapshot
- a chain whose stored state already matches is skipped, so repeated runs write nothing; event IDs are deterministic
- a snapshot in a live state may lift a refund the Service still holds (missed REFUND_REVERSED)
- replay goes through the caller's parser, oldest first; already-delivered notifications are duplicates, older-than-stored are stale; unverifiable records are counted Failed
**Verified:** unit tests incl. ordering races, 2026-10-15; live Sandbox reconcile applied then unchanged, replay applied 3 / ignored 1 / failed 0, 2026-10-15.

## Apple — refund consumption responses
**What:** `SendConsumptionInfo` (V2) and `NewConsumptionHandler` answer CONSUMPTION_REQUEST.
**DoD:**
- nothing is sent unless `CustomerConsented` is true; the zero value is never sent
- invalid combinations (undelivered with a percentage, percentage on a subscription, prorated without a percentage) are refused before any request
- the handler answers 200 when sent, not consented, or past the 12 h deadline; 500 when the responder or Apple failed inside the deadline (production Apple redelivers); 401 bad signature; every other notification reaches the wrapped handler unchanged
**Verified:** unit tests with a fake App Store, 2026-10-15; not called against the real API.
