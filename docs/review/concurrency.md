# Concurrency-safety review

Scope: `Service.Process`, the memory and Postgres stores, the Apple verifier, the Google parser and the `httpx` handlers.
Date: 2026-10-14. Verified with the race detector (Go 1.27.2) and Postgres 17.

## Test runs

- `go test -race -count=3 -short ./...` passes.
- `go test -race -count=2 -p=1 -tags=integration ./...` passes.

## Probes

Probe tests are in `docs/review/probes/`, behind the build tag `probe`, so normal runs skip them. Several of them fail on purpose, because they demonstrate the findings below.

```
go test -race -tags=probe ./docs/review/probes/
go test -race -tags='probe integration' -p=1 ./docs/review/probes/   # needs Docker
```

The stress workload has 6 chains and 9 events per chain, each delivered 4 times and shuffled, over 64 goroutines. Every third event supersedes the next chain, which makes the supersedes cross. The invariants checked are:

- one row per chain;
- `version` equals the number of applied events;
- the final state is the newest applied event;
- `fn` runs at most once per event ID;
- the number of `processed_events` rows equals the number of distinct IDs;
- no deadlock within 90 s.

## Findings

### 1. Data race: `AppleRootCAG3Pool()` returns the shared pool (`apple/verifier.go:37-46`)

- It returns the process-wide `*x509.CertPool`, and `x509.CertPool` has no lock.
- A caller that runs `AddCert` or `AppendCertsFromPEM` on it races with `Verifier.Verify` (`apple/verifier.go:157`).
- It also changes trust for every default verifier in the process.
- Probe `TestProbe_ApplePoolRace` reports `WARNING: DATA RACE` between `AppendCertsFromPEM` and `Verify`. `TestProbe_ApplePoolSharedMutable` shows a default verifier accepting a foreign CA.
- Fix: return `defaultPool.Clone()`, and document that a pool passed to `WithRootPool` must not be modified afterwards.

### 2. Lost update: a supersede for an unseen chain is consumed (`service.go:82`)

- If the new chain's event arrives before any event for the old chain, the retire step returns `nil, nil`. The store records `<id>#supersede` as processed and persists nothing.
- The old chain's later event creates it as `active` and nothing retires it. Redelivering the new chain's event does not help, because the retire event now counts as a duplicate.
- Probe `TestProbe_SupersedeBeforeOldChainSeen_IsLost` ends with `OLD` active next to `NEW`.
- Fix options:
  - Do not record the retire event when the old chain does not exist. This needs a new sentinel from `fn` that rolls back without recording.
  - Or persist a tombstone, such as an expired row at that `OccurredAt`, so later older events return `ErrStale`.

### 3. Atomicity gap: supersede is two transactions (`service.go:51-55`, `:71-95`)

- Between retiring the old chain and applying the new one, the user has no active chain.
- Probe `TestProbe_SupersedeIntermediateStateVisible` shows a concurrent `IsActive` returning false in that window.
- If the second step fails, the user stays without access until redelivery. `TestProbe_SupersedePartialFailureAndRetry` shows a retry recovers cleanly: `OLD` ends at version 2 and `NEW` at version 1.
- Fix: apply the new chain first, then retire the old one. The retire can be retried and a short overlap is harmless.

### 4. Hardening: `40001` is not retried (`store/postgres/store.go:136`)

- Under the default Read Committed isolation, 25 rounds of stress had no errors, no deadlocks and no violated invariants.
- If the database or pool defaults to Repeatable Read or Serializable, `Apply` returns SQLSTATE `40001`. In my runs about 50 calls per round failed. No data was corrupted.
- Probes `TestPGStress_RepeatableRead` and `TestPGStress_Serializable` fail on these errors. The handler would answer 500 and rely on the webhook retry.
- Fix: begin the transaction with `pgx.TxOptions{IsoLevel: pgx.ReadCommitted}`. Optionally also retry on `40001` and `40P01`.

### 5. Documentation only

- `google/parser.go:109` stores the request body slice in `Event.Raw` without copying, unlike the Apple parser. It only matters if the caller reuses the buffer.
- `Event.ID` must be unique across sources, because dedupe is keyed on `event_id` alone.
- The memory store's `processed` map grows without bound.
- Calling the store from inside `fn` deadlocks the memory store and starves the Postgres pool.
- Events with equal `OccurredAt` are applied in arrival order, so the final state at a tie is nondeterministic.

## Checked and found safe

- **Deadlock between crossing supersedes:** none. Each `Apply` takes one advisory lock, and the two supersede steps are separate transactions. This held with 64 goroutines and with a pool of only 2 connections (`TestPGStress_TinyPool`).
- **Duplicate event racing the first delivery:** the `ON CONFLICT (event_id) DO NOTHING` insert makes the second delivery wait, then return `ErrDuplicate`.
- **Concurrent `Migrate`:** 16 separate pools migrating at once all succeed (`TestPG_MigrateConcurrent`).
- **Panic in `fn`:** the connection and the chain lock are released (`TestPG_PanicInFnReleasesConnAndLock`).
- **Context cancel mid-apply and while waiting on the lock:** no connection leak, and the wait returns at the deadline (`TestPG_CtxCancelMidApply`, `TestPG_LockWaitHonoursCtx`).
- **Memory store:** one mutex, and `Get` and `ListByUser` return value copies. `Entitlement` has no slices or maps, so nothing aliases internal state.

## Concurrency contract

| Type | Concurrent use |
|---|---|
| `Service`, `memory.Store`, `postgres.Store` (on a `pgxpool`), `apple.Verifier`, `apple.Parser`, `google.Parser`, `google.AndroidPublisherFetcher`, `httpx` handlers, `entitlements.Apply` | Safe after construction, because they are immutable |
| Injected `timeFn` closures, the `WithRootPool` pool | Safe only if the caller keeps them safe and does not modify the pool |
| Pool returned by `AppleRootCAG3Pool()` | Not safe today (finding 1) |
| `mocks` and `httpx/mocks` | Not safe, for tests only |
