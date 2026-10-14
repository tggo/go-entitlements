# Go best-practice review

Scope: public-API quality, errors, context, logging, time, resources, tests, go.mod, Postgres store.
Checks run: `go vet` clean, `golangci-lint` 0 issues, `go test -race ./...` passes (unit tests; integration tests not run, need Docker).
Panics and the 500-loop in items 1 and 3 were reproduced with a scratch test outside the repo.

## Blockers

1. **A permanently invalid event makes the handlers answer 500, so stores retry forever.**
   `service.go:45-47` returns a plain `fmt.Errorf` for an event missing ID, Source, OriginalTxID or State. It is not a sentinel, so `httpx/httpx.go:97` hits `default` and answers 500. Reproduced with a Google voided-purchase push without `purchaseToken` (`google/parser.go:143` builds an empty `OriginalTxID`): HTTP 500. Same path for an Apple notification without `originalTransactionId` or `notificationUUID`.
   Fix: add `ErrInvalidEvent` in `entitlements.go`, wrap it in `service.go:46` (use `errors.New`/`%w`, there are no format args), map it to 200 or 400 in `httpx.process`. Also reject an empty `PurchaseToken` in `fromVoided` as `ErrMalformed`.

2. **`NewPubSubAuthenticator` silently drops audience validation when `audience == ""`** (`httpx/auth.go:19,33`). `idtoken.Validate` skips the `aud` check for an empty audience (`idtoken/validate.go:160`). A misconfigured empty env var then accepts any Google-signed token for the right service account email.
   Fix: return an error (or panic, `Must`-style) in the constructor when `audience` or `email` is empty.

## Should-fix

3. **Options and constructors are not nil-safe.** All panic (verified): `WithLogger(nil)` then `Process` (`service.go:26`, used at `:62`); `WithTimeFn(nil)` then `IsActive` (`service.go:23`, `:101`), same for `apple.WithTimeFn(nil)` (`apple/verifier.go:70`); a nil `Option` in any variadic list; `google.NewParser(nil)` on the first subscription push (`google/parser.go:153`).
   Fix: skip nil options, keep the default when a nil logger or clock is passed, return an error from `NewParser` for a nil fetcher. `httpx` already handles a nil logger (`httpx/apple.go:30`); the root package should too.

4. **A `Processor` returning `(nil, nil)` panics the handler** (`httpx/httpx.go:90`, `ent.State`). `Processor` is a public interface. Fix: guard `ent != nil`.

5. **zap is in the public API** of the root package and httpx: `WithLogger(*zap.Logger)` (`service.go:26`), `NewAppleHandler`/`NewGoogleHandler(..., *zap.Logger, ...)` (`httpx/apple.go:29`, `httpx/google.go:34`). Every importer, including apple-only users, gets zap (9 packages in `go list -deps .`).
   Recommendation: switch to `*slog.Logger` (stdlib, zero-dep, idiomatic for go 1.26); zap users bridge with `zapslog`. Do it before v1 (README already says the API may change). If zap stays, take a small logger interface instead.

6. **`google` and `httpx` pull far more than needed.** `go list -deps`: root, `apple` and the stores are 80-113 non-std packages with 0 `google.golang.org/api`; `google` and `httpx` are about 305-310 with 64 grpc and 27 otel packages. Causes: `google/fetcher.go:8` imports `androidpublisher`; `google/parser.go:11` imports `googleapi` only for `*googleapi.Error`; `httpx/auth.go:10` imports `idtoken`; `httpx/google.go:11` imports `google`. An Apple-only user importing `httpx` gets all of it.
   Fix: move the fetcher to `google/androidpublisher` and have it classify errors into `ErrPurchaseNotFound`/`ErrFetchConfig`/`ErrFetchTransient`, so `google` needs no Google SDK; move the Pub/Sub authenticator to `httpx/pubsubauth`; keep shared sentinels where `httpx` can import them without the SDK.

7. **Postgres migration bookkeeping collides with other tools** (`store/postgres/store.go:66`). `CREATE TABLE IF NOT EXISTS schema_migrations (name ...)` silently skips when golang-migrate or goose already created a `schema_migrations` with other columns; the following `SELECT ... WHERE name` then fails. Tables `entitlements` and `processed_events` are unprefixed, with no schema or prefix option.
   Fix: rename to `entitlements_schema_migrations`; consider a `WithSchema` or table-prefix option, and document it.

8. **`processed_events.raw` is stored forever with no retention** (`migrations/001_init.sql`, `store.go:148`): full webhook payloads, possibly PII or tokens, unbounded growth.
   Fix: document it; add `Store.Prune(ctx, before time.Time)` or an option to skip storing `raw`.

9. **Concurrency contract is undocumented.** No thread-safety comment on `Service`, `Store`, `Verifier`, `apple.Parser`, `google.Parser`. All appear safe; say so. `Store.Apply` docs should state that `fn` must be fast, do no I/O and not re-enter the store: `memory.Store` runs `fn` under its mutex (`store/memory/store.go:53,65`); Postgres runs it inside a transaction holding an advisory lock and a row lock.

10. **No bounded timeout or client control on outbound HTTP** (`google/fetcher.go:24`, `httpx/auth.go:20`). The fetcher uses whatever client is inside `androidpublisher.Service`; the handler passes `r.Context()` without deadline; the authenticator uses `idtoken`'s default validator with a non-injectable client.
    Fix: document the requirement, add a per-fetch timeout (`WithFetchTimeout`), and accept an `*idtoken.Validator` or HTTP client in the authenticator.

11. **CI lacks `-race`, `go mod tidy` check and `go vet`.** `Makefile:5` `test-unit` has no `-race`; the stores are concurrency-sensitive. Add `-race` and a tidy check. The `ci.yml` Go pin `1.26` matches `go.mod`.

12. **Mocks are public packages and `dockertest` is a direct requirement.** `mocks/`, `google/mocks/`, `httpx/mocks/` widen the exported surface and make `testify` a non-test dependency of those packages. `go.mod` carries `ory/dockertest` and about 60 `// indirect` lines (docker, runc) used only by integration tests; consumers do not compile them but `go.sum` and `go mod graph` bloat.
    Fix: move mocks under `internal/`; better, make `store/postgres` its own module holding `pgx` and `dockertest`.

## Nits

13. **State and source constants have no per-const docs** (`entitlements.go:14-30`; the only undocumented exported identifiers in an AST scan). Document what `grace`, `billing_retry` and `paused` mean for access. The lint preset `comments` hides this.

14. **No `Example_` functions.** Add examples for `Apply` (pure state machine), `Service.IsActive` with the memory store, and mounting `httpx`.

15. **Zero `t.Parallel()` calls.** Only `t.Helper` is used (12 uses). Add `t.Parallel` to the independent tests (`statemachine_test.go`, parsers). There is no `time.Sleep`/`time.After` anywhere, so timing is deterministic.

16. **Postgres errors are returned bare** (`store/postgres/store.go:58,64,138,144,153` and others). Wrap with context, e.g. `fmt.Errorf("postgres: begin apply: %w", err)`.

17. **`go.mod` says `go 1.26.0`.** Use the minor: `go mod edit -go=1.26`. No `toolchain` directive is present, good.

18. **Raw body handling is inconsistent.** `apple/parser.go:33` copies the body into `Event.Raw`; `google/parser.go:109` aliases the caller's slice. Copy in both, or document.

19. **`apple.Verifier.Verify` has no cap on x5c length** (`apple/verifier.go:136`); every cert is parsed before the chain check. Bounded by the 1 MiB body limit, but a cap of about 5 is cheap. A payload JSON decode failure after a valid signature (`:189`) is not wrapped as invalid (gives 400); fine, but undocumented.

20. **Event IDs are deduplicated globally, not per source** (`processed_events.event_id` PK, `store.go:148`; memory store keys by `ev.ID`). A collision between an Apple UUID and a Pub/Sub messageId is practically impossible, but keying on `(source, event_id)` is free.

21. **`AppleRootCAG3Pool` panics on a corrupt embedded cert** (`apple/verifier.go:41`). Acceptable for an embedded asset covered by tests; add a comment saying so.

22. **`ErrNoExpiry` and `ErrTestPurchase` wrap `ErrUnsupported`/`ErrIgnored`** (`google/parser.go:25,28`). `errors.Is` works, but `httpx` logs them at Warn as "not applicable" and answers 200. Consider a loud log for `ErrNoExpiry`: it silently drops a granting event until the next notification.

## What is already good

Context goes through every I/O call, and only `examples/` uses `context.Background()`. Request bodies are capped with `MaxBytesReader`. Sentinels use `%w`, and `errors.Is` works through multi-`%w` wrapping. Time is UTC with ms parsing in one place (`msToTime`, `Millis.Time`) and the clock is injectable. Migrations run in one transaction under an advisory lock and are idempotent; SQL is fully parameterized; every `Apply` path rolls back via `defer`. All exported identifiers are documented except the constants in item 13.
