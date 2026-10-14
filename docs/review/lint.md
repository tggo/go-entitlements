# Lint review (2026-10-14)

Build, `go vet` (default and `integration` tags), `go mod tidy -diff` and `go mod verify` are clean. No bugs found, only cleanups.
Tools: golangci-lint v2.13.2 (strict config, all linters minus noise), staticcheck 0.8.2 (`-checks all`), govulncheck (latest), fieldalignment, shadow.
Recommended config: `docs/review/golangci-recommended.yml` (still reports 62 issues; fix items below first or leave `wrapcheck`/`shadow` off).

## Real findings

1. `go.mod:3` is `go 1.26.0`. Use the minor only: `go mod edit -go=1.26 -toolchain=none`. A patch pin forces every consumer of this library onto that patch.
2. Local Go 1.27.1 has 11 stdlib vulnerabilities fixed in 1.27.2 (net/http, http2, crypto/tls, html/template, net/textproto), reached via `examples/server/main.go:144`. This is the toolchain, not repo code: `brew upgrade go`. Two more stdlib findings (net/http, os) are in imported but uncalled code. `golang.org/x/crypto` v0.58.0 (GO-2026-5932) has no fix and is not called. The installed `govulncheck` binary was built with go1.25 and fails on this module; use `go run golang.org/x/vuln/cmd/govulncheck@latest`.
3. revive `exported`:
   - `apple/verifier.go:37` `AppleRootCAG3Pool` stutters; `RootCAG3Pool` is a breaking rename, decide before the first tag.
   - `entitlements.go:15,28`: exported `State*` / `Source*` consts have no comment.
   - `google/parser.go:30,33,37`: `Err*` comments should start with the identifier name.
4. wrapcheck: `store/postgres/store.go` lines 59-239 (about 16 sites) return raw pgx errors with no context; wrap them, e.g. `fmt.Errorf("postgres: begin: %w", err)`. Same at `service.go:68,99`.
5. staticcheck SA1019 at `apple/apple_test.go:283` (`CertPool.Subjects`). The `//nolint:staticcheck` only silences golangci-lint; standalone staticcheck needs `//lint:ignore SA1019 reason`, or compare pools with `Equal`.
6. perfsprint: `service.go:46` `fmt.Errorf` without verbs should be `errors.New`. Tests: `e2e_integration_test.go:292`, `store/postgres/store_integration_test.go:322`, `store/memory/store_test.go:89` (`strconv.Itoa`).
7. govet shadow (8 sites, `err` in `if err := ...`): `apple/verifier.go:127,138,157`, `store/postgres/store.go:63,66,143`, `examples/server/main.go:62,69`. Fix or leave `shadow` off.
8. cyclop: `google/parser.go:148` `fromSubscription` scores 21 (limit 20). Extract the best-line-item selection.
9. Test-only mechanical fixes:
   - noctx: `httptest.NewRequest` to `NewRequestWithContext` at `httpx/auth_internal_test.go:39,50`, `httpx/httpx_test.go:26`.
   - intrange: `store/memory/store_test.go:109`, `e2e_integration_test.go:303,311`, `store/postgres/store_integration_test.go:239,273`.
   - prealloc: `apple_flow_test.go:79`.
   - unparam: `apple/identity_test.go:47,129`, `store/postgres/store_integration_test.go:109`.
   - revive unused-parameter: `apple/identity_test.go:62,80,97`, `google/fetcher_test.go:57,67`, `service_test.go:29`.
10. forcetypeassert: `apple/identity_test.go:35,68,71` (unchecked assertion on fixture map; cosmetic).
11. thelper / testifylint (about 16 + 37): missing `t.Helper()` in `runChain` (`apple_flow_test.go`) and `newSvc` (`service_test.go:19`); `testing.TB` param should be named `tb` in `internal/testutil/apple_ca.go`; testifylint mostly wants `require.Error` / `ErrorIs`.

## Noise (not real)

- staticcheck ST1000 on `mocks/` (5 files): generated code.
- gocritic `hugeParam` (9) / `rangeValCopy` (5): `Event` (208 B) and `Entitlement` (144 B) are passed by value on purpose. `httpNoBody` (3) is test-only. Both disabled in the recommended config.
- fieldalignment (about 20 structs): 8-80 bytes saved on DTOs and test structs; churn.
- nilnil (6): `nil, nil` is the documented "skip" result of the `Apply` callback (`service.go:82`) and test stores.
- exhaustive `google/parser.go:199`: the switch intentionally handles only Grace and Active; `default-signifies-exhaustive` clears it.
- contextcheck `examples/server/main.go:141`: `context.Background()` is needed for shutdown because the parent context is already cancelled.
- gosec (7): G304/G301/G306 in `examples/`, `sign-fixture`, `internal/testutil` (fixed fixture paths).
- errchkjson (4), dogsled (3), mnd (8), revive import-shadowing / confusing-naming: test or style noise.
- wrapcheck on `google/types.go:34,121` (`json.Unmarshal` inside `UnmarshalJSON`): wrapping adds nothing.
- Stale indirect modules (docker, containerd, runc via testcontainers): not flagged by govulncheck.
- Strict run: 228 issues (default tags), 261 (integration); about 30 distinct real or arguable items above.
