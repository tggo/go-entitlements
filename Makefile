.PHONY: test test-unit test-integration lint generate-mocks cover run-example

test: test-unit test-integration

test-unit:
	go test -short -count=1 ./...

# Needs Docker (Postgres 17 via dockertest).
test-integration:
	go test -p=1 -count=1 -tags=integration ./...

lint:
	golangci-lint run ./...
	golangci-lint run --build-tags=integration ./...

generate-mocks:
	mockery

# Coverage over unit + integration tests, mocks and test helpers excluded.
cover:
	go test -p=1 -count=1 -tags=integration -coverpkg=./... -coverprofile=cover.out ./...
	grep -v -e '/mocks/' -e '/internal/testutil/' -e '/examples/' cover.out > cover.filtered.out
	go tool cover -func=cover.filtered.out | tail -1

run-example:
	docker compose up -d --wait
	DATABASE_URL=postgres://postgres:postgres@localhost:5432/entitlements?sslmode=disable \
	APPLE_BUNDLE_ID=com.example.app go run ./examples/server
