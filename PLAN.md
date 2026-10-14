# go-entitlements — work plan

One Go library that turns App Store Server Notifications v2 and Google Play
Real-Time Developer Notifications into a single, ordered, idempotent
subscription state ("entitlement") per user and product.

Gap it fills (checked 2026-10-14): Go libraries cover Apple only
(richzw/appstore, izniburak/appstore-notifications-go); the only unified
two-store package is PHP/Laravel (vipertecpro/mobile-entitlements).

## Goals (MVP, one night)

1. Verify Apple `signedPayload` JWS against the Apple Root CA chain — never
   trust the `x5c` header blindly.
2. Decode Google RTDN Pub/Sub push messages, then fetch the real state from
   `purchases.subscriptionsv2.get` (the notification itself carries no state).
3. Normalize both into one `Event` and run it through a pure state machine.
4. Idempotency (dedupe by Apple `notificationUUID` / Pub/Sub `messageId`) and
   ordering (an older event never overwrites a newer one: Apple `signedDate`,
   Google `eventTimeMillis`).
5. Pluggable `Store` with in-memory and Postgres implementations.
6. `http.Handler` per store, ready to mount.

Non-goals for MVP: receipt validation (legacy verifyReceipt), App Store Server
API client beyond what tests need, Stripe/web PSPs, consumables.

## Package layout

```
entitlements.go        core types: Event, Entitlement, State, Source, errors
statemachine.go        pure Apply(current, event) -> (next, changed, err)
service.go             Service: Process(ctx, Event) — dedupe, order, apply, store
interfaces.go          Store, Verifier interfaces (only interface file)
apple/                 JWS verifier, payload types, mapper -> entitlements.Event
google/                RTDN decoder, SubscriptionsV2 fetcher iface, mapper
store/memory/          in-memory Store
store/postgres/        pgx Store + migrations (embed), row-level locking
httpx/                 AppleHandler, GoogleHandler (net/http)
internal/testutil/     fixture loader, test Apple CA + signer for fixtures
testdata/              real-shaped JSON fixtures for both stores
examples/server/       runnable demo wired to Postgres
```

## Core model

```go
type State string // active | grace | billing_retry | paused | expired | refunded | revoked
type Source string // apple | google

type Event struct {
    ID          string    // notificationUUID / messageId — idempotency key
    Source      Source
    UserID      string    // appAccountToken (Apple) / obfuscatedExternalAccountId (Google)
    OriginalTxID string   // originalTransactionId / purchaseToken (linked chain)
    ProductID   string
    State       State
    ExpiresAt   time.Time
    OccurredAt  time.Time // signedDate / eventTimeMillis — ordering key
    Raw         []byte
}

type Entitlement struct {
    UserID, ProductID, OriginalTxID string
    Source     Source
    State      State
    ExpiresAt  time.Time
    UpdatedAt  time.Time // = OccurredAt of last applied event
    Version    int64
}
func (e Entitlement) Active(now time.Time) bool // active|grace and not expired
```

Rules:
- Duplicate `Event.ID` → no-op, `ErrDuplicate` reported as success to the store
  webhook (return 200 so stores stop retrying).
- `OccurredAt` older than `Entitlement.UpdatedAt` → recorded, not applied
  (`ErrStale`), still 200.
- Google `linkedPurchaseToken`: new token supersedes old one; old one marked
  superseded, entitlement continues on the new chain.
- Time comes from an injected `timeFn` for testability.

## Apple mapping (notificationType/subtype → State)

SUBSCRIBED, DID_RENEW, OFFER_REDEEMED, DID_CHANGE_RENEWAL_PREF → active;
DID_FAIL_TO_RENEW+GRACE_PERIOD → grace; DID_FAIL_TO_RENEW → billing_retry;
GRACE_PERIOD_EXPIRED → billing_retry; EXPIRED → expired; REFUND → refunded;
REVOKE → revoked; DID_CHANGE_RENEWAL_STATUS → state unchanged, only flag;
TEST → ignored. Unknown types → `ErrUnsupported`, logged, 200.

## Google mapping (subscriptionsv2 subscriptionState → State)

ACTIVE → active; IN_GRACE_PERIOD → grace; ON_HOLD → billing_retry;
PAUSED → paused; CANCELED → active until expiry; EXPIRED → expired;
PENDING_PURCHASE_CANCELED → expired. Voided purchases (RTDN
`voidedPurchaseNotification`) → refunded. testNotification → ignored.

## Tests — same conventions as Words-Platform-Backend

- Unit tests: `go test -short ./...`, no Docker, mockery mocks
  (`.mockery.yaml`, `mocks.NewX(t)`, never manual mocks).
- Integration tests: files `*_integration_test.go`, `//go:build integration`,
  testify `suite.Suite`, `SetupSuite` starts a real container with
  `ory/dockertest/v3` (Postgres 17), `TearDownTest` truncates, `TearDownSuite`
  stops. Skip on `testing.Short()`. Logger `zaptest.NewLogger(t)`.
  (The private `go-common-lb/dockertool` is not used — this is public OSS.)
- Bug fixes are TDD: red test first.
- Fixtures: Apple payloads are signed in tests by a generated test CA so the
  full verification path runs; verifier takes the root pool as an option.
- Must-have cases: duplicate delivery, out-of-order delivery, stale event after
  refund, grace → renew, upgrade via linkedPurchaseToken, concurrent delivery of
  two events for the same subscription (Postgres: `SELECT ... FOR UPDATE`),
  tampered JWS, wrong root.
- Coverage > 85% on non-mock code.

## Tooling

- `Makefile`: `test`, `test-unit`, `test-integration`, `lint`,
  `generate-mocks`, `cover`.
- `docker-compose.yaml`: Postgres for the example server.
- `.golangci.yml`, GitHub Actions CI (unit + integration with Docker).
- Go 1.26 in go.mod (public library: minor only, no toolchain line).
- License: MIT.

## Work split (agents)

Phase 0 (lead, done before agents): go.mod, PLAN.md, core types + interfaces.

Phase 1 — parallel, disjoint directories:
- A. `statemachine.go`, `service.go`, `store/memory`, `store/postgres` +
  integration suite.
- B. `apple/` + Apple fixtures + test CA signer.
- C. `google/` + Google fixtures + mocked subscriptionsv2 fetcher.

Phase 2 — one agent: `httpx/`, `examples/server`, end-to-end integration test
(HTTP → service → Postgres), Makefile, CI, README with the "five traps" story.

Phase 3 — review: run everything, `ruslan-review`-style verification, fix.

## Done means

`make test-unit && make test-integration && make lint` green locally;
`examples/server` accepts a fixture POST and the entitlement shows up in
Postgres; README explains usage in under 60 seconds of reading.
