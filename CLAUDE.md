# go-entitlements — agent notes

Public Go library (github.com/tggo/go-entitlements). App Store Server Notifications V2 and Google Play
RTDN become one ordered, idempotent entitlement per purchase chain. Commands and test conventions:
`CONTRIBUTING.md`. Behaviour contract: `DEFINITION_OF_DONE.md`. Explainer site: skill `site-simulator`.

## Rules learned the hard way

- **Real data never enters git.** Real signed Apple notifications (from the owner's app) live in
  `testdata/real/` (gitignored); tests over them skip when absent and pin the clock to the newest
  `signedDate` (`testutil.LatestSignedDate`) because the leaf cert expires. App Store Connect keys are
  read from env (`ASC_KEY_PATH` outside the repo), behind build tag `live`. Before every commit:
  `git status --short` and `git grep -I -nE "BEGIN PRIVATE KEY" -- . ':!testdata/prior-art'`.
  The keys under `testdata/prior-art/` are Apple's public MIT test keys (allowlisted in `.gitleaks.toml`).
- **Run real data before trusting tests.** The first pass over five real notifications found two bugs
  the synthetic suite missed (missing `appAccountToken`, `ONE_TIME_CHARGE`). Synthetic fixtures encode
  our assumptions; real payloads test them.
- **Mine other libraries' issues before claiming coverage.** `docs/prior-art/` (21 libs + Nakama) is the
  checklist of real-world failures; a new mapping or rule gets checked against it.
- **Cite the store docs, not memory.** Every mapping row has a doc source; the domain review found five
  wrong rows written from intuition (refund resurrection via DID_CHANGE_RENEWAL_STATUS, grace cut by
  status/pref changes, Google grace over-extension, cancelled pending upgrade, upgrade without user).
- **Safe defaults are part of the API.** Sandbox/TestFlight/Google test purchases are ignored unless
  `WithAllowSandbox()` (owner's decision 2026-10-15). Google push needs an authenticator or explicit
  `WithInsecureNoAuth()`. Bundle ID and package name are required constructor arguments.
- **HTTP answers decide retries.** 200 for anything a retry cannot fix (duplicate, stale, ignored,
  invalid event, unknown purchase), 500 only when a retry can help. A wrong 500 is an infinite retry loop.
- **Ordering:** "newest `signedDate`/`eventTime` wins" holds (Apple keeps `signedDate` on retries), with
  two exceptions encoded in `statemachine.go`: per-transaction refunds (`RefundedTransactions`) and
  `Restores` (REFUND_REVERSED). Only SUBSCRIBED, DID_RENEW and REFUND_REVERSED may lift refunded/revoked.
- **A Store is correct only if `storetest.Run` and `storetest.RunStress` pass.** Assert what was
  persisted (re-`Get`), not what the callback returned — mutation testing caught that gap.
- **Coverage is not evidence; mutants are.** 94% coverage hid 25+ untested checks. After non-trivial
  test work run `make mutate` and hand-mutate `store/postgres` under the integration tag.
- **Keep `google`, `httpx` and the root free of heavy deps.** Google SDK only in `google/androidpublisher`
  and `httpx/pubsubauth`; check with `go list -deps ./google ./httpx | grep -c google.golang.org/api` (= 0).
- **Logging:** `*slog.Logger` via `WithLogger`, never purchase tokens or URLs containing them
  (`google.RedactToken`).

## Tooling gotchas (Go 1.27.2 host)

- golangci-lint must be built with the local Go (Homebrew 2.14.0 works); an older binary fails typecheck.
- mockery 2.53.x needed a rebuild with newer `golang.org/x/tools`; mocks live in `internal/mocks/`,
  regenerate only through `.mockery.yaml` (`make generate-mocks`).
- gremlins needs a high `--timeout-coefficient` (set in the Makefile) or every mutant "times out".
- `govulncheck`: run via `go run golang.org/x/vuln/cmd/govulncheck@latest` (an old installed binary fails).
- `go.mod` stays `go 1.26.0` (Google API modules require the patch form).

## Working in parallel agents

Split by directory ownership (root+apple+store+storetest / google+httpx / apple/serverapi / site).
Agents do not commit; the lead runs `make test-unit test-integration lint test-site` on the merged tree
and commits. A change to observable behaviour regenerates the site goldens (skill `site-simulator`).

## Releases

Tag `vX.Y.Z` on main → `.github/workflows/release.yml` runs tests and creates the GitHub release.
Write release notes by hand for user-facing changes; state honestly what was and was not verified
(Google has not run on live Play traffic).


<!-- git-dod:start -->
## Definition of Done + git-as-log (skill `git-dod`)

Two artifacts, never conflated: `DEFINITION_OF_DONE.md` = the living **SPEC**
("how it must behave now", one entry per feature, edited in place); **git history
= the LOG** — per-file "why" via `git log --follow -- <file>`. There is no
separate worklog file.

- Behaviour change → update that feature's `DEFINITION_OF_DONE.md` entry **in the
  same commit** as the code. **Never invent a DoD — if the criteria are unclear, ASK.**
- `feat` / `fix` commit messages MUST carry `Why:` and `Expected:` lines, and every
  commit must use a canonical type (enforced by the `commit-msg` hook).
- Hook missing? `make hooks` (or `bash install.sh --yes` from a git-dod checkout).
  Never bypass with `--no-verify`; fix the message instead.

Full playbook: skill `git-dod` (`.claude/skills/git-dod/SKILL.md`).
<!-- git-dod:end -->

## Inbox (optional store-first layer)

- `inbox` holds the types, `ProcessStored` and the status rules; `inbox/memory` (tests and demos only) implements it,
  `inboxtest.Run` is the conformance suite a consumer's durable Inbox must pass; there is deliberately no Postgres inbox. `httpx.WithInbox` is the handler side. Default behaviour must stay
  "parse and apply inside the request": new inbox code never changes that path.
- A status is final unless it is `received` or `failed`; `ProcessStored` returns an error exactly for the
  non-final ones. A new outcome needs a status, a row in the README and a case in the aggregate-precedence test.
- Never store `Authorization` or other credentials: `inbox.SafeHeaders` is an allowlist, every implementation
  calls it in `Save`, and the conformance suite checks it.
