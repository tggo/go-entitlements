#!/usr/bin/env bash
# git-dod :: Stop hook (soft nag, Claude Code only)
#
# If the latest commit (HEAD) changed code but did NOT touch the Definition of
# Done file, surface a one-time, non-blocking reminder. A marker inside .git
# keeps it to once per offending commit (no loops, not noisy).
# Any error / nothing to flag -> exit 0 silently.
#
# Per-repo settings live in .git-dod.conf at the repo root.
set -u

DOD_FILE="DEFINITION_OF_DONE.md"
CODE_PATHS=""   # space-separated path prefixes; empty = use the extension heuristic
CODE_EXTS="go ts tsx js jsx mjs py rb rs java kt swift php c h cc cpp cs sql sh vue svelte html css scss templ proto"

root="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
cd "$root" 2>/dev/null || exit 0
[ -f .git-dod.conf ] && . ./.git-dod.conf

head="$(git rev-parse HEAD 2>/dev/null)" || exit 0
gitdir="$(git rev-parse --absolute-git-dir 2>/dev/null)" || exit 0
marker="$gitdir/.dod_reminded"

# Already handled this commit? stay silent.
[ -f "$marker" ] && [ "$(cat "$marker" 2>/dev/null)" = "$head" ] && exit 0

files="$(git show --name-only --pretty=format: "$head" 2>/dev/null)" || exit 0

# Record that we inspected this commit, so we fire at most once for it.
printf '%s' "$head" > "$marker" 2>/dev/null || true

code_changed=0
if [ -n "$CODE_PATHS" ]; then
  pattern="$(printf '%s' "$CODE_PATHS" | tr ' ' '|')"
  printf '%s\n' "$files" | grep -qE "^($pattern)/" && code_changed=1
else
  pattern="$(printf '%s' "$CODE_EXTS" | tr ' ' '|')"
  printf '%s\n' "$files" \
    | grep -vE '(_test\.|\.test\.|\.spec\.|^tests?/|/tests?/|^vendor/|^node_modules/)' \
    | grep -qE "\.($pattern)$" && code_changed=1
fi

dod_changed=0
printf '%s\n' "$files" | grep -qxF "$DOD_FILE" && dod_changed=1

if [ "$code_changed" = "1" ] && [ "$dod_changed" = "0" ]; then
  short="$(printf '%s' "$head" | cut -c1-9)"
  printf '{"systemMessage":"⚠️ git-dod: commit %s changed code but %s was not updated. Add or update the feature entry — or confirm there is no observable behaviour change."}\n' "$short" "$DOD_FILE"
fi
exit 0
