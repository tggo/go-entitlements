
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
