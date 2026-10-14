# Third-party test fixtures

Everything here was copied from, or derived from, the public repositories named in each
subdirectory's `SOURCE.md`, under their MIT licences (the licence text is in `LICENSE` next to the files).
`google-issue-derived/` is hand-written and not copied.

The `*.key`, `*.p8` and `*.pem` files under `app-store-server-library-java/certs/` are **public test keys
from Apple's MIT-licensed client libraries**. They sign nothing real: they belong to Apple's test CA
(`testCA` -> `testIntermediate` -> `testLeaf`), which no Apple service trusts. They are kept so tests can
sign JWS with a chain that has, and one that lacks, Apple's marker OIDs. Secret scanners will flag them;
`.gitleaks.toml` at the repository root allowlists this directory.
