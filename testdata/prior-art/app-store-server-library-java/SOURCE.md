# app-store-server-library-java fixtures

- Repo: https://github.com/apple/app-store-server-library-java
- Commit: 8533331183841de6ea0c555aef2e763cd1951cf3
- License: MIT (Copyright 2023 Apple Inc.)
- Path: `src/test/resources/certs/`
- Root: these are test certificates made by the library authors (testCA -> testIntermediate -> testLeaf),
  not Apple Root CA - G3. Test keys are published in the source repo and have no value.

Use them to build negative signing tests (a valid chain missing the Apple marker OIDs):
| File | What |
|---|---|
| testCA.der/.pem/.key | test root |
| testIntermediate.pem/.key, testLeaf.pem/.key | valid chain with OIDs 1.2.840.113635.100.6.2.1 (intermediate) and 1.2.840.113635.100.6.11.1 (leaf) |
| testInvalidOIDIntermediate.pem, testInvalidOIDLeaf.pem | same chain with a wrong marker OID (checked with openssl: intermediate carries 1.2.840.113635.100.6.2.2, leaf carries 1.2.840.113635.100.6.11.2) |
| testInvalidIntermediateLeaf.pem | leaf carrying the right OID (6.11.1); its use in their tests is an invalid-intermediate case, issuer relationship not checked by us |
| testSigningKey.p8 | API key used by their client tests, irrelevant for us |
