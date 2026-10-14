# go-entitlements

**[Illustrated explainer with an interactive simulator](https://tggo.github.io/go-entitlements/)**

A Go library that turns App Store Server Notifications V2 and Google Play Real-Time Developer Notifications into one ordered, idempotent subscription state per user and product. You mount two HTTP handlers, and your code asks one question: does this user currently have access to this product.

Status: v0. The core, both parsers, the in-memory and Postgres stores and the HTTP handlers are tested end to end, but the API may change between minor versions until v1. Changes are listed in the release notes.

## Why

Each store has traps that cause wrong access decisions, and most examples online fall into at least one. The library handles these five:

1. **Unverified `x5c`.** Apple signs notifications as JWS and puts the certificate chain in the `x5c` header. Verifying the signature against that chain proves nothing, because an attacker supplies the chain too. The library checks that the chain ends at Apple Root CA - G3 (embedded), that the leaf and intermediate carry Apple's marker OIDs, and only then checks the signature. Bundle ID and environment can be pinned.
2. **Google RTDN carries no state.** The Pub/Sub message only says that something happened to a purchase token. The library calls `purchases.subscriptionsv2.get` and builds the state from that response, not from the notification type.
3. **Duplicates.** Both stores redeliver. Events are deduplicated by Apple `notificationUUID` and Pub/Sub `messageId`, in the same transaction that updates the entitlement.
4. **Out-of-order delivery.** A renewal can arrive after the refund that followed it. An event older than the last applied one (Apple `signedDate`, Google `eventTimeMillis`) is recorded but not applied.
5. **Upgrades via `linkedPurchaseToken`.** On Google, an upgrade or resubscribe creates a new purchase token. The new token supersedes the old one, and the old chain is closed, so a user never has two active entitlements for one purchase.

Also handled: during a billing grace period `ExpiresAt` is extended to the grace period end, so `Entitlement.Active` keeps granting access while the store retries the charge.

## Install

```
go get github.com/tggo/go-entitlements
```

Requires Go 1.26. For Postgres, run `store.Migrate` once at startup; it is idempotent and safe to call from several instances. The tables are `entitlements`, `processed_events` and `entitlements_schema_migrations`; use a dedicated schema (search_path) if the names collide with yours.

## Quickstart

Apple only, in-memory store. The snippets below are compiled as Examples in `quickstart_example_test.go`, and a test fails if the README drifts from them.

<!-- example:Example_appleOnly -->
```go
ctx := context.Background()
logger := slog.Default()

svc := entitlements.NewService(memory.New(), entitlements.WithLogger(logger))

verifier, err := apple.NewVerifier("com.example.app", // the bundle ID is required
	apple.WithEnvironment(entitlements.EnvironmentProduction)) // reject Sandbox and TestFlight
if err != nil {
	log.Fatal(err)
}
mux := http.NewServeMux()
mux.Handle("/webhooks/apple", httpx.NewAppleHandler(apple.NewParser(verifier), svc, httpx.WithLogger(logger)))
go func() { log.Fatal(http.ListenAndServe(":8080", mux)) }()

// Later, in your own handlers: does this user have access?
ok, err := svc.IsActive(ctx, "7e3fb20b-4cdb-47cc-936d-99d65f608138", "com.example.pro.monthly")
if err != nil {
	log.Fatal(err)
}
log.Println("has access:", ok)
```

The same `Service` on Postgres:

<!-- example:Example_postgres -->
```go
ctx := context.Background()
logger := slog.Default()

pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
	log.Fatal(err)
}
defer pool.Close()
store := postgres.New(pool)
if err := store.Migrate(ctx); err != nil { // idempotent, safe from several instances
	log.Fatal(err)
}
svc := entitlements.NewService(store, entitlements.WithLogger(logger))

list, err := svc.ListByUser(ctx, "7e3fb20b-4cdb-47cc-936d-99d65f608138")
if err != nil {
	log.Fatal(err)
}
for _, e := range list {
	log.Println(e.ProductID, e.State, e.ExpiresAt)
}
```

Google Play. Create a Pub/Sub push subscription that points at the endpoint and sends an OIDC token (audience = the endpoint URL, service account = the one you pass to `pubsubauth.New`). The service account behind `androidpublisher.NewService` needs permission to view financial data for the app in the Play Console. A push endpoint without authentication is refused at construction: anyone who knew a purchase token could otherwise forge a refund.

Acknowledgement: Google refunds subscriptions that are not acknowledged within 3 days. With `httpx.WithAcknowledger` the handler acknowledges a purchase (`purchases.subscriptions.acknowledge`) after its entitlement is stored, only when Google reports the purchase as pending and it grants access. If the acknowledge fails transiently the push answers 500; Pub/Sub redelivers, the event is a duplicate, and the acknowledge is tried again. Without the option, acknowledge from your app or backend yourself. The service account needs the "Manage orders and subscriptions" permission.

<!-- example:Example_google -->
```go
ctx := context.Background()
logger := slog.Default()
svc := entitlements.NewService(memory.New(), entitlements.WithLogger(logger))

playAPI, err := play.NewService(ctx) // credentials from GOOGLE_APPLICATION_CREDENTIALS
if err != nil {
	log.Fatal(err)
}
fetcher := androidpublisher.NewFetcher(playAPI)
parser, err := google.NewParser(fetcher, "com.example.app")
if err != nil {
	log.Fatal(err)
}
// The Pub/Sub push subscription must send an OIDC token for this audience
// and service account; the handler refuses to start without a check.
auth, err := pubsubauth.New("https://example.com/webhooks/google", "push@my-project.iam.gserviceaccount.com")
if err != nil {
	log.Fatal(err)
}
// WithAcknowledger acknowledges new purchases once they are stored; Google
// refunds purchases that are not acknowledged within 3 days.
handler, err := httpx.NewGoogleHandler(parser, svc, httpx.WithLogger(logger),
	httpx.WithPushAuthenticator(auth), httpx.WithAcknowledger(fetcher))
if err != nil {
	log.Fatal(err)
}
http.Handle("/webhooks/google", handler)
```

`Service` has `Process` (used by the handlers), `IsActive(ctx, userID, productID)`, `Get(ctx, source, originalTxID)` and `ListByUser(ctx, userID)`.

The user is identified by `appAccountToken` on Apple and `obfuscatedExternalAccountId` on Google. Set it when the purchase is made in your app and events arrive attributed. Treat the token as a hint from the client, not as authentication: a client can send someone else's UUID.

If your app does not set it, an event arrives with an empty `UserID`. It is still applied, keyed by the transaction (`originalTransactionId` or purchase token), and the service logs a warning. A user on a chain that is later retired by a Google upgrade is inherited automatically. For the rest there are two ways to attach users:

<!-- example:Example_attributeUsers -->
```go
ctx := context.Background()
logger := slog.Default()

svc := entitlements.NewService(memory.New(),
	entitlements.WithLogger(logger),
	// Path 1: look the user up when an event arrives without one, for example
	// in your originalTransactionId -> user table. An error makes the store retry.
	entitlements.WithUserResolver(func(ctx context.Context, ev entitlements.Event) (string, error) {
		return lookupUser(ctx, ev.Source, ev.OriginalTxID) // "" if unknown yet
	}),
	// React to a committed change, e.g. to tell the user or flip a feature flag.
	entitlements.WithOnChange(func(ctx context.Context, before, after *entitlements.Entitlement) {
		logger.InfoContext(ctx, "entitlement changed", "user", after.UserID, "state", after.State)
	}))

// Path 2: the app reports the purchase right after it happens. This is
// idempotent; a chain that already belongs to another user returns
// entitlements.ErrUserConflict and is never reassigned.
_, err := svc.LinkUser(ctx, entitlements.SourceApple, "2000000000001", "user-42")
if errors.Is(err, entitlements.ErrUserConflict) {
	log.Println("this purchase belongs to another account")
}

// Erasure request: delete the user's entitlements and stored payloads.
if _, err := svc.ForgetUser(ctx, "user-42"); err != nil {
	log.Fatal(err)
}
```

- `WithUserResolver` is asked for every event that has no user. Use it when you keep your own transaction-to-user table.
- `LinkUser(ctx, source, originalTxID, userID)` attaches a user to an existing chain; call it when your app reports a purchase. It is atomic and idempotent, returns `ErrUserConflict` for a chain owned by someone else, and `ErrNotFound` before the first notification arrived (retry after it).
- A chain's owner is never changed afterwards: a later event with another user ID, or none, leaves it alone (the service logs a warning for a different one). `ListByUser("")` returns nothing.
- `WithOnChange` runs after each committed change, and `ForgetUser` deletes a user's entitlements and stored payloads (see Privacy below).

`Entitlement.AutoRenew` is the last known renewal setting (Apple `autoRenewStatus`, nil if never reported); an active entitlement with `AutoRenew` false is a churn signal.

### Sandbox and test purchases

Sandbox and TestFlight purchases on Apple and license-tester purchases on Google are free. `Event.Environment` says where a purchase was made (`Production`, `Sandbox`, `Xcode`, `LocalTesting`, `GoogleTest`; empty means production). By default `Service.Process` returns `ErrIgnored` for non-production events and `IsActive` skips non-production entitlements. Build the service with `entitlements.WithAllowSandbox()` in test or staging deployments. Pin the Apple verifier with `apple.WithEnvironment` as well.

If production and TestFlight send notifications to the same URL, TestFlight testers' events are ignored by default. Point the App Store Connect Sandbox URL at a staging service built with `WithAllowSandbox()`, or enable it on the shared one if free tester access is acceptable.

A runnable server with Postgres, both webhooks and `GET /entitlements?user_id=` is in `examples/server`. To try it locally without Apple:

```
docker compose up -d --wait
go run ./examples/sign-fixture -out demo subscribed did_renew   # throwaway CA + signed bodies, dated now
DATABASE_URL='postgres://postgres:postgres@localhost:5432/entitlements?sslmode=disable' \
APPLE_BUNDLE_ID=com.example.app APPLE_ROOT_CA_PEM=demo/root.pem go run ./examples/server
curl -X POST --data @demo/subscribed.json localhost:8080/webhooks/apple
curl 'localhost:8080/entitlements?user_id=7e3fb20b-4cdb-47cc-936d-99d65f608138'
```

`APPLE_ROOT_CA_PEM` replaces Apple's root with your own and is for this local demo only. Never set it in production.

## App Store Server API

`apple/serverapi` calls Apple's App Store Server API: subscription statuses, transaction history, transaction info, order lookup, test notifications and notification history. Requests are signed with your App Store Connect key (ES256 JWT, a fresh one per request), 429 and 5xx answers are retried honoring Retry-After, and every signed value Apple returns is verified with `apple.Verifier` and pinned to your bundle ID and environment. The key is never logged.

### Repairing a lost webhook

<!-- example:Example_reconcile -->
```go
ctx := context.Background()

key, err := os.ReadFile("AuthKey_ABC123DEFG.p8")
if err != nil {
	log.Fatal(err)
}
client, err := serverapi.New(serverapi.Config{
	IssuerID:      "57246542-96fe-1a63-e053-0824d011072a",
	KeyID:         "ABC123DEFG",
	PrivateKeyPEM: key,
	BundleID:      "com.example.app",
	Environment:   entitlements.EnvironmentProduction,
})
if err != nil {
	log.Fatal(err)
}
svc := entitlements.NewService(memory.New())

// 1. A customer says they paid but have no access: ask Apple.
results, err := serverapi.Reconcile(ctx, client, svc, "2000000000000001")
for _, r := range results {
	fmt.Println(r.Event.OriginalTxID, r.Outcome)
}
if err != nil {
	log.Print(err)
}

// 2. After an outage, re-feed every notification Apple could not deliver.
verifier, err := apple.NewVerifier("com.example.app", apple.WithEnvironment(entitlements.EnvironmentProduction))
if err != nil {
	log.Fatal(err)
}
report, err := serverapi.ReplayNotificationHistory(ctx, client, apple.NewParser(verifier), svc,
	time.Now().Add(-48*time.Hour), time.Now(), serverapi.ReplayOnlyFailures())
fmt.Printf("applied %d, duplicates %d, stale %d, failed %d\n", report.Applied, report.Duplicate, report.Stale, report.Failed)
if err != nil {
	log.Print(err)
}
```

`Reconcile` asks Apple for the customer's current status and feeds one event per subscription chain to the Service. Events are ordered by Apple's own `signedDate`, so a snapshot never overwrites a webhook Apple signed later, and a chain that already matches is skipped, so running it repeatedly writes nothing. After an outage, `serverapi.ReplayNotificationHistory` re-feeds the notifications Apple could not deliver (the example above does both); ones you already received count as duplicates.

### Refund requests (CONSUMPTION_REQUEST)

When a customer asks Apple for a refund, Apple sends CONSUMPTION_REQUEST and waits 12 hours for you to say how much of the purchase was used. You may answer only if the customer agreed to share that data; otherwise send nothing. `apple/serverapi` does the sending, and you supply the facts:

<!-- example:Example_consumptionResponder -->
```go
var (
	client   *serverapi.Client // from serverapi.New
	verifier *apple.Verifier   // from apple.NewVerifier
	next     http.Handler      // httpx.NewAppleHandler(parser, svc)
)
responder := serverapi.ConsumptionResponderFunc(func(ctx context.Context, req serverapi.ConsumptionRequest) (serverapi.ConsumptionInfo, error) {
	consented, err := lookUpConsent(ctx, req.UserID, req.OriginalTransactionID)
	if err != nil {
		return serverapi.ConsumptionInfo{}, err // transient: Apple redelivers
	}
	if !consented {
		return serverapi.ConsumptionInfo{}, serverapi.ErrConsentRequired // nothing is sent
	}
	return serverapi.ConsumptionInfo{
		CustomerConsented:     true,
		DeliveryStatus:        serverapi.DeliveryDelivered,
		SampleContentProvided: true,
		RefundPreference:      serverapi.RefundDecline,
	}, nil
})
http.Handle("/apple", serverapi.NewConsumptionHandler(next, serverapi.ConsumptionHandlerConfig{
	Verifier: verifier, Responder: responder, Sender: client,
}))
```

The wrapper answers CONSUMPTION_REQUEST itself and passes every other notification to the handler it wraps. The library never sets consent: the zero value means "no consent" and is never sent. If your responder or Apple fails inside the 12 hours, the endpoint answers 500 so Apple redelivers (production only; the first retry comes an hour later). Past the deadline it answers 200 and stops.

## State mapping

Normalized states: `active`, `grace`, `billing_retry`, `paused`, `expired`, `refunded`, `revoked`. `Active(now)` is true for `active` and `grace` before `ExpiresAt`.

Apple (`notificationType` / `subtype`):

| Notification | State |
|---|---|
| SUBSCRIBED, DID_RENEW | active (the only events besides REFUND_REVERSED that can lift a refunded or revoked chain) |
| DID_FAIL_TO_RENEW with GRACE_PERIOD | grace, until `gracePeriodExpiresDate` |
| DID_FAIL_TO_RENEW, GRACE_PERIOD_EXPIRED | billing_retry |
| EXPIRED | expired |
| REFUND | refunds one transaction (`transactionId`): the chain ends only if that was the one granting access, otherwise the refund is recorded and the chain runs on; a renewal that arrives after the refund of the transaction it replaced still applies |
| REVOKE | revoked |
| REFUND_REVERSED | restores the chain; state from `data.status` |
| DID_CHANGE_RENEWAL_STATUS, DID_CHANGE_RENEWAL_PREF, OFFER_REDEEMED, RENEWAL_EXTENDED | state from `data.status` (1 active, 2 expired, 3 billing retry, 4 grace, 5 revoked), else from `expiresDate` vs `signedDate`; never lift a refund |
| any event whose transaction has `revocationDate` | refunded (revoked for REVOKE) |
| TEST, ONE_TIME_CHARGE, RENEWAL_EXTENSION, REFUND_DECLINED, PRICE_INCREASE, PRICE_CHANGE, CONSUMPTION_REQUEST, METADATA_UPDATE, MIGRATION, EXTERNAL_PURCHASE_TOKEN, RESCIND_CONSENT, CHILD_CONSENT_REVOKED | ignored (answer it with `serverapi.NewConsumptionHandler`, see above) |
| anything else | unsupported |

An active or grace event without an expiry date is refused (`apple.ErrNoExpiry`, HTTP 200 and an error log) instead of granting access forever.

Google (`subscriptionsv2.get` `subscriptionState`; the notification type is not used):

| Subscription state | State |
|---|---|
| ACTIVE | active |
| CANCELED | active (until expiry) |
| IN_GRACE_PERIOD | grace |
| ON_HOLD | billing_retry |
| PAUSED | paused |
| EXPIRED, PENDING_PURCHASE_CANCELED | expired |
| PENDING | ignored |
| voidedPurchaseNotification | refunded |
| testNotification | ignored |
| other states, other notification kinds | unsupported |

## HTTP response policy

| Situation | Status | Why |
|---|---|---|
| Applied, duplicate, stale, ignored (TEST), unsupported type | 200 | The store should stop retrying. Logged. |
| Google notification for another package name | 200 | Redelivery cannot fix it. Logged. |
| Invalid Apple signature, failed Pub/Sub token | 401 | Logged at error level. |
| Malformed body | 400 | Logged at error level. |
| Not POST | 405 | |
| Body over the limit (default 1 MiB, `WithMaxBodyBytes`) | 413 | |
| Store or Play API failure | 500 | The store retries. |

A 4xx means the request was rejected for good, not that it will succeed on retry, so watch the error logs.

## Testing

```
make test-unit          # go test -short -race, no Docker
make test-integration   # real Postgres 17 through Docker (dockertest), -race
make stress             # heavy concurrency stress on both stores
make fuzz FUZZTIME=30s  # Go fuzz targets for the Apple and Google parsers and the state machine
make lint               # golangci-lint
make cover              # coverage, mocks and test helpers excluded
make generate-mocks     # mockery
```

Apple payloads in tests are signed by a generated test CA, so the full verification path runs; the verifier takes the root pool as an option. Apple's own libraries' test JWS verify against their test CA. Google calls go through a mocked `SubscriptionsFetcher`. `e2e_integration_test.go` exercises HTTP, service and Postgres together.

The author also ran five real signed notifications from a live app (one Production TEST, four Sandbox) through the parser and the state machine. Those files are private and not in the repository, so no CI run or outside reader can reproduce that; `apple/real_fixtures_test.go` skips when they are absent. Google has never been run against live Play traffic.

## Writing your own store

Implement `entitlements.Store` (the numbered contract is in its doc comment) and run the conformance suite from your tests:

```go
func TestConformance(t *testing.T) {
    storetest.Run(t, func(t *testing.T) entitlements.Store {
        return mystore.New(resetDatabase(t)) // fresh, empty store per subtest
    })
}
```

`storetest` uses only the standard library, runs its subtests one after another, and covers atomic apply, per-source dedupe, recorded-not-applied outcomes, retry after errors, supersede tombstones, concurrency and context cancellation. The memory and Postgres stores in this repo run the same suite.

## Known limitations

- Google: a subscription with add-ons is stored as one entitlement per line item (`purchaseToken#productId`). A purchase with a single item keeps the plain `purchaseToken` key. When a purchase gains or loses an add-on, the row under the old key shape is not rewritten and ends at its own stored expiry; immediate item-based revocation is not tracked.
- `go.mod` says `go 1.26.0`, not `1.26`: the Google API dependencies require it.
- Privacy: the Postgres store keeps each webhook body in `processed_events.raw` so that events can be audited. It can contain account identifiers; call `Store.Prune` on a schedule, and `Service.ForgetUser` for an erasure request (it clears the payloads of that user's chains and deletes the entitlements, keeping event IDs so redelivered webhooks stay duplicates).

Other libraries' issues this one was tested against are in [docs/prior-art](docs/prior-art/README.md). Contributing and security reporting: [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md).

## Not in scope

Legacy `verifyReceipt`, Stripe and other web payment providers, and consumables.

## License

MIT
