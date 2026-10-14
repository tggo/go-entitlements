# Contributing

```
make test-unit          # fast, no Docker, -race
make test-integration   # needs Docker: Postgres 17 through dockertest
make lint               # golangci-lint v2, built with your Go version
make stress             # heavy concurrency stress on both stores
make fuzz FUZZTIME=30s  # fuzz targets
make generate-mocks     # mockery, after changing an interface
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
