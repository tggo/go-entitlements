.PHONY: test test-unit test-integration stress fuzz lint generate-mocks cover run-example

test: test-unit test-integration

test-unit:
	go test -short -count=1 ./...

# Needs Docker (Postgres 17 via dockertest).
test-integration:
	go test -p=1 -count=1 -tags=integration ./...

# Heavy concurrency stress on the memory and Postgres stores, with the race
# detector (Docker needed for Postgres). Lighter: STORETEST_STRESS=light.
stress:
	STORETEST_STRESS=heavy go test -race -count=1 -p=1 -tags=integration -v -run 'TestStress|TestStoreSuite/TestStress' ./store/...

# Go fuzzing, FUZZTIME per target (not run in CI).
FUZZTIME ?= 30s
fuzz:
	go test -run '^$$' -fuzz '^FuzzApply$$' -fuzztime $(FUZZTIME) .
	go test -run '^$$' -fuzz '^FuzzAppleParse$$' -fuzztime $(FUZZTIME) ./apple
	go test -run '^$$' -fuzz '^FuzzGoogleParse$$' -fuzztime $(FUZZTIME) ./google

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

# --- git-dod auto-install ----------------------------------------------------
# ANY `make` invocation installs the commit-msg hook if it is missing. Idempotent:
# once the symlink exists this is a cheap `test -L` no-op. Because developers run
# `make test` / `lint` / `build` before pushing, the hard gate is effectively always
# on — nobody has to remember an install step.
#
# It also pins a LOCAL core.hooksPath, which overrides a GLOBAL one (a developer
# with `git config --global core.hooksPath ~/.git-hooks` would otherwise have the
# repo hook silently shadowed and never fire).
#
# NOTE: local hooks are always bypassable (`git commit --no-verify`); the advisory
# layer is CI — see .github/workflows/verify-commits.yml (non-blocking).
__install_hooks := $(shell \
	gd="$$(git rev-parse --absolute-git-dir 2>/dev/null)"; \
	if [ -n "$$gd" ] && [ ! -L "$$gd/hooks/commit-msg" ] && [ -f .githooks/commit-msg ]; then \
		mkdir -p "$$gd/hooks"; \
		git config --local core.hooksPath "$$gd/hooks" 2>/dev/null; \
		ln -sf "$(CURDIR)/.githooks/commit-msg" "$$gd/hooks/commit-msg" 2>/dev/null; \
		chmod +x .githooks/commit-msg .claude/hooks/dod_reminder.sh 2>/dev/null; \
	fi; true)

.PHONY: hooks
hooks:
	@hd="$$(git rev-parse --absolute-git-dir)/hooks"; \
	git config --local core.hooksPath "$$hd"; \
	mkdir -p "$$hd"; \
	ln -sf "$(CURDIR)/.githooks/commit-msg" "$$hd/commit-msg"; \
	chmod +x .githooks/commit-msg .claude/hooks/dod_reminder.sh 2>/dev/null || true; \
	echo "✓ git-dod: commit-msg -> .githooks/commit-msg (hooksPath=$$hd)"
# --- end git-dod -------------------------------------------------------------
