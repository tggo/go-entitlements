# Contributing

```
make test-unit          # fast, no Docker, -race
make test-integration   # needs Docker: Postgres 17 through dockertest
make lint               # golangci-lint v2, built with your Go version
make stress             # heavy concurrency stress on both stores
make fuzz FUZZTIME=30s  # fuzz targets
make generate-mocks     # mockery, after changing an interface
make mutate             # mutation testing with gremlins (unit-testable packages), a few minutes
```

- Write the failing test first for a bug fix.
- A new `Store` must pass `storetest.Run`. A behaviour change updates `DEFINITION_OF_DONE.md` in the same commit.
- Apple payloads in tests are signed by a generated CA (`internal/testutil`). Do not commit real signed
  notifications, purchase tokens or account identifiers.
- The README quickstart is compiled: edit `quickstart_example_test.go` and paste the body into the README.
  `TestReadmeSnippetsMatchExamples` fails if they differ.

Dependencies: `go.mod` says `go 1.26.0` because the Google API modules require it. `ory/dockertest` is a
test-only dependency of the root module; module graph pruning keeps it out of consumers' builds. It would
move to a nested module only if that stopped being enough.

## Mutation testing

`make mutate` runs gremlins (version pinned in the Makefile) over the packages whose tests run without Docker.
It does not cover `store/postgres`, whose tests need the integration tag; mutate that package by hand
(`go test -tags=integration ./store/postgres` after editing a query) and check that a test fails.

Efficacy target: at least 95% of covered mutants killed, and every survivor explained. Measured on
2026-10-15 over the root package, `apple/`, `store/memory` and the conformance suite (the `google/` and `httpx/`
tests were excluded because their suites were being edited): 159 killed, 0 survived, 100% efficacy, 62 mutants
not covered (the Postgres package, plus lines gremlins cannot map). Hand-written mutants (removed checks,
swapped states, dropped SQL columns) are all killed: 33 across the state machine, service, Apple verifier
and mapper, and the Postgres `ON CONFLICT`, `Prune` and `processed_events` paths.

The timeout coefficient in the Makefile matters: gremlins derives each mutant's time limit from the baseline
run, and a cached baseline is so fast that, without it, almost every mutant "times out" and is reported
as neither killed nor lived.
Gremlins reports some lines as "not covered" (multi-condition `switch` cases) although tests run them;
that is a limit of its coverage mapping.
