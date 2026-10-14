# go-entitlements

A Go library that turns App Store Server Notifications V2 and Google Play Real-Time Developer Notifications into one ordered, idempotent subscription state per user and product. You mount two HTTP handlers, and your code asks one question: does this user currently have access to this product.

Status: early, MVP. The core, both parsers, the in-memory and Postgres stores and the HTTP handlers are tested end to end, but the API may still change before v1.

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

Requires Go 1.26. For Postgres, run `store.Migrate` once at startup; it is idempotent and safe to call from several instances.

## Quickstart

```go
pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
store := postgres.New(pool)
_ = store.Migrate(ctx)
svc := entitlements.NewService(store, entitlements.WithLogger(log))

appleVerifier, _ := apple.NewVerifier("com.example.app") // bundle ID is required; add apple.WithEnvironment("Production") to reject sandbox
appleParser := apple.NewParser(appleVerifier)
play, _ := androidpublisher.NewService(ctx) // credentials from GOOGLE_APPLICATION_CREDENTIALS
googleParser := google.NewParser(google.NewAndroidPublisherFetcher(play), google.WithPackageName("com.example.app"))

mux := http.NewServeMux()
mux.Handle("/webhooks/apple", httpx.NewAppleHandler(appleParser, svc, log))
mux.Handle("/webhooks/google", httpx.NewGoogleHandler(googleParser, svc, log,
    httpx.WithPushAuthenticator(httpx.NewPubSubAuthenticator(audience, pushServiceAccountEmail))))

ok, _ := svc.IsActive(ctx, userID, "com.example.pro.monthly")
```

The user is identified by `appAccountToken` on Apple and `obfuscatedExternalAccountId` on Google. Set it when the purchase is made in your app; notifications without it cannot be attributed and are acknowledged and logged.

A runnable server with Postgres, both webhooks and `GET /entitlements?user_id=` is in `examples/server`. To try it locally without Apple:

```
docker compose up -d --wait
go run ./examples/sign-fixture -out demo subscribed did_renew   # throwaway CA + signed bodies
DATABASE_URL='postgres://postgres:postgres@localhost:5432/entitlements?sslmode=disable' \
APPLE_BUNDLE_ID=com.example.app APPLE_ROOT_CA_PEM=demo/root.pem go run ./examples/server
curl -X POST --data @demo/subscribed.json localhost:8080/webhooks/apple
curl 'localhost:8080/entitlements?user_id=7e3fb20b-4cdb-47cc-936d-99d65f608138'
```

`APPLE_ROOT_CA_PEM` replaces Apple's root with your own and is for this local demo only. Never set it in production.

## State mapping

Normalized states: `active`, `grace`, `billing_retry`, `paused`, `expired`, `refunded`, `revoked`. `Active(now)` is true for `active` and `grace` before `ExpiresAt`.

Apple (`notificationType` / `subtype`):

| Notification | State |
|---|---|
| SUBSCRIBED, DID_RENEW, OFFER_REDEEMED, DID_CHANGE_RENEWAL_PREF | active |
| DID_FAIL_TO_RENEW with GRACE_PERIOD | grace |
| DID_FAIL_TO_RENEW, GRACE_PERIOD_EXPIRED | billing_retry |
| EXPIRED | expired |
| REFUND | refunded |
| REVOKE | revoked |
| DID_CHANGE_RENEWAL_STATUS | derived from the transaction: active if it expires after the notification, else expired |
| TEST | ignored |
| anything else | unsupported |

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
make test-unit          # go test -short, no Docker
make test-integration   # real Postgres 17 through Docker (dockertest)
make lint               # golangci-lint
make cover              # coverage, mocks and test helpers excluded
make generate-mocks     # mockery
```

Apple payloads in tests are signed by a generated test CA, so the full verification path runs; the verifier takes the root pool as an option. Google calls go through a mocked `SubscriptionsFetcher`. `e2e_integration_test.go` exercises HTTP, service and Postgres together: subscribe, renew and refund, duplicates, out-of-order delivery, a Google upgrade, and concurrent deliveries.

## Known limitations

- Apple: if a refund for an older transaction arrives before the renewal that superseded it, the chain is refunded and the later renewal is dropped as stale. Fixing this needs per-transaction tracking.
- Google: the grace-period end is not documented in `subscriptionsv2`. During `IN_GRACE_PERIOD` access lasts until the later of `expiryTime` and notification time plus `WithGraceDuration` (default 7 days). Not yet confirmed against a live response.
- Google: the library never acknowledges purchases. Acknowledge within 3 days or Google refunds them.
- Apple: tested against real signed Sandbox and Production notifications from a live app. Google: tested against documented payloads and fixtures derived from public issues, not live traffic.

See [docs/prior-art](docs/prior-art/README.md) for the issues in other libraries this one was tested against.

## Not in scope

Legacy `verifyReceipt`, Stripe and other web payment providers, consumables, and an App Store Server API client.

## License

MIT
