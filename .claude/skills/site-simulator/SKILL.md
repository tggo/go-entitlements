---
name: site-simulator
description: Keep the GitHub Pages explainer (site/) and its interactive simulator in sync with the Go library. Use after any change to statemachine.go, service.go, apple/mapper.go, google/parser.go or httpx response codes; when TestSiteScenarios or site/check-scenarios.js fails; when adding a simulator card, scenario, diagram or page section; or when asked to update the site, explainer, simulator or GitHub Pages.
---

# Site simulator

`site/` is a static explainer deployed by `.github/workflows/pages.yml` on push to main (paths `site/**`).

| File | Role |
|---|---|
| `site/index.html`, `style.css` | page, inline SVG diagrams (each with `<title>`/`<desc>`), light+dark |
| `site/engine.js` | JS port of the rules: `statemachine.go`, `service.go`, `apple/mapper.go`, `google/parser.go`; source files named in comments |
| `site/app.js` | UI only; calls `Engine.deliver` / `Engine.snapshot` |
| `site/scenarios.json` | single source: cards (store messages) + scenarios, each step with `expect` (HTTP code, stamp, stored chains, access on dates) |
| `site_scenarios_test.go` | replays every card/scenario through the REAL parsers, handlers, Service and memory store (only the Play fetcher and Pub/Sub auth are fakes) |
| `site/check-scenarios.js` | replays the same JSON through `engine.js` (node, CI + before each deploy) |

## When library behaviour changes

1. `go test -run TestSiteScenarios -update-site .` — regenerates `expect` blocks from the Go code.
2. Read the `git diff site/scenarios.json`: every changed outcome must be one you intended. An
   unexpected change is a library bug, not a golden to accept.
3. Port the rule to `engine.js` by reading the Go source line by line — never from a summary.
4. `make test-site` until 0 mismatches.
5. Mutation-check the port: change each new JS rule on purpose; if `check-scenarios.js` still passes,
   add a card (hidden cards are fine) until it fails.
6. Update page copy, diagrams and the limitations section so no claim contradicts `DEFINITION_OF_DONE.md`.

## When the Go side and the port disagree

Go wins. Fix `engine.js`. If Go looks wrong, stop and report — do not edit library code from here.

## Adding a card or scenario

- Apple cards are signed at test time by a throwaway CA; forged cards by an untrusted second CA.
- Give each Apple billing period its own `txn` (`t-jan`, `t-feb`, …) so refunds name the right period.
- Google cards need the subscriptionsv2 state the fake fetcher returns.
- Visible scenarios teach one idea each; hidden ones exist to kill mutants.

## Design rules (from the frontend-design brief used to build it)

One memorable element (the simulator with stamped outcomes); everything else flat: no shadows,
no gradients, 4px corners, 68ch text column. Palette tokens live in `style.css` `:root` with dark
overrides. No ALL-CAPS eyebrows, no "→" on buttons, no "A · B · C" meta strings. Copy is plain and
honest: say what was not verified. Respect `prefers-reduced-motion`; visible focus rings; no
horizontal page scroll at 390px.

## Verify visually

Chrome automation cannot resize the window here; use playwright-core with local Chrome from a
scratch directory (not the repo) at 1440 light, 1440 dark and 390, click through changed scenarios,
check the console is clean.
