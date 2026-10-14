---
name: git-dod
description: >
  Definition-of-Done practice with git as the work-log. Use when committing a
  behaviour change, writing a commit message, updating DEFINITION_OF_DONE.md, or
  fixing the DoD hooks. DEFINITION_OF_DONE.md = the living SPEC (how it must
  behave now); git history = the LOG (commits carry Why:/Expected:). feat/fix
  commits need Why:/Expected: lines — enforced by the commit-msg hook.
---

# git-dod — Definition of Done, with git as the work-log

Two questions, two artifacts — never conflate them (that is the mistake this
practice exists to prevent):

| Question | Artifact |
|---|---|
| **"How must it behave right now?"** | `DEFINITION_OF_DONE.md` — the **SPEC** |
| **"What changed, why, when, where?"** | **git history** — the **LOG** |

There is deliberately **no WORKLOG.md** — git already is the log. Because the
rationale lives in the commit, you get per-file causal history for free:

```bash
git log --follow -- <path>         # every change to a file, and why
git log -p --follow -- <path>      # …with diffs
git blame <path> → git show <sha>  # why a specific line exists
```

## DEFINITION_OF_DONE.md — the SPEC

Repo root. **One entry per feature, edited in place.** It is not a changelog, so
it never grows unbounded — when behaviour changes you rewrite the entry.

- **What** — one line: the feature / behaviour.
- **DoD** — observable acceptance criteria (what a human or QA can check).
- **Verified** — how it was confirmed (a real measurement) + date, or `—`.

**Never invent a DoD.** If the criteria are unclear or ambiguous, **ASK the user**
and record their answer. A guessed criterion is worse than none.

Rule: any change that adds or alters **observable behaviour** updates its entry
**in the same commit** as the code. Pure refactor / test / chore → no entry (say
so explicitly in the commit or the reply).

## Commit message — the LOG

`feat` and `fix` commits MUST include `Why:` and `Expected:` lines in the body.
The scope is the diff, so do not restate the file list:

```
fix(comic/parallax): keep the snap stage inside the PWA safe area

Why: in standalone PWA the top of the image and the speech bubbles hid under the island.
Expected: no panel or bubble is clipped by the island; desktop unchanged.

- snap-scroller offset into the safe area (top inset; height − top/bottom insets)
- cache-bust parallax.css v11→v12
```

## Commit types

The `commit-msg` hook rejects any type outside the configured list — that is what
catches divergence (`update:`, `feture:`). The list lives in `.git-dod.conf`
(`TYPES=`); the default set is:

| Type | Use for | Why:/Expected: | DoD entry |
|---|---|---|---|
| `feat` | new user-facing behaviour | **required** | usually yes |
| `fix` | bug fix (behaviour change) | **required** | usually yes |
| `perf` | performance, same behaviour | optional | if limits/behaviour change |
| `refactor` | restructure, no behaviour change | no | no |
| `docs` | docs / comments only | no | no |
| `test` | tests only | no | no |
| `style` | formatting, no logic | no | no |
| `build` | build system, deps, tooling | no | no |
| `ci` | CI / pipeline config | no | no |
| `chore` | misc maintenance | no | no |
| `revert` | reverts a prior commit | no | reflect the revert |

Every non-merge commit uses `type(scope): summary`. Repos that prefix subjects
with an issue key (`[ABC-123]feat(scope): …`) are handled by the hook — see
`JIRA_PREFIX` in `.git-dod.conf`. Bypass once with `[skip-checks]` in the message
(emergencies only, and say why in the body).

## Enforcement layers

1. **`commit-msg` hook** (`.githooks/commit-msg`, git-native, **hard**) — blocks
   unknown types and `feat`/`fix` without `Why:`/`Expected:`. Applies to every
   `git commit`, not just the agent's.
2. **Stop-hook** (`.claude/hooks/dod_reminder.sh`, Claude-only, **soft**) — after a
   commit that changed code without touching the DoD file, one non-blocking nag.
3. **Advisory CI** (`.github/workflows/verify-commits.yml`, optional) — re-runs the
   same hook over the PR's commits and reports violators as warnings. Never blocks;
   do not add it to branch-protection required checks.
4. **CLAUDE.md rule** + the commit step of your workflow — the documentation layer.

## Working loop for an agent

1. Before coding: read `DEFINITION_OF_DONE.md` for the area you are about to touch.
2. Implement.
3. Behaviour changed? Update / add the entry — in the same commit. Unsure about
   the criteria? **Ask, don't guess.**
4. Commit with `type(scope): summary` + `Why:` / `Expected:` for feat/fix.
5. If the hook rejects the commit, fix the message — do **not** reach for
   `--no-verify` or `[skip-checks]`.
6. After verifying behaviour for real (a run, a measurement, a screenshot), fill in
   the **Verified** line with what you measured and the date.

## Repair / reinstall

```bash
make hooks                 # if the repo has the Makefile snippet
# or
bash install.sh --yes      # from a git-dod checkout, run inside the target repo
```

Verify the hard gate really fires:

```bash
f=$(mktemp); echo "feat(x): t"   >"$f"; bash .githooks/commit-msg "$f"; echo exit=$?  # expect 1
echo "chore(x): t"  >"$f"; bash .githooks/commit-msg "$f"; echo exit=$?               # expect 0
echo "update(x): t" >"$f"; bash .githooks/commit-msg "$f"; echo exit=$?; rm "$f"      # expect 1
```

If the Stop-hook is not firing after editing `.claude/`, open `/hooks` once or
restart Claude Code so the watcher reloads.

Upstream kit and rationale: <https://github.com/tggo/git-dod>
