# Security policy

This library verifies store signatures and decides who has paid access, so a flaw can grant or deny access
wrongly. Please report suspected vulnerabilities privately and do not open a public issue.

- Use GitHub's "Report a vulnerability" (private security advisory) on this repository.
- Include the version or commit, what you did, what you expected and what happened. A failing test or a
  signed fixture that reproduces it helps most.
- You can expect an acknowledgement within a few days, then a fix or a reasoned "not a vulnerability".

Supported versions: the latest tagged release. The library is v0 and fixes are not backported.

Not vulnerabilities in this library: `APPLE_ROOT_CA_PEM` in `examples/server` (a documented local-demo
override), the public test keys under `testdata/prior-art/`, and missing authentication on a handler you
built with `httpx.WithInsecureNoAuth`.
