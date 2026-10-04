# Security Policy

## Supported versions

The latest release is the supported one. Fixes land on `main` and go out in the
next tag.

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Use GitHub's private vulnerability reporting instead:
[Security → Report a vulnerability](https://github.com/iamnikolie/oko/security/advisories/new).
That opens a private advisory visible only to the maintainers.

Include what you did, what happened, and the impact you think it has. Expect a
first response within a week — this is a spare-time project, not a product with
an on-call rotation.

## Scope notes

Some things are known and by design rather than vulnerabilities:

- **The debugging port is open to local processes.** Each profile's Chrome
  listens on a random port bound to 127.0.0.1. Anything running as you (or any
  local process that can reach loopback) can drive that browser and read its
  cookies. Keep banking and primary accounts out of oko profiles.
- **Proxy credentials are stored in plain text** in
  `~/.oko/profiles/<name>/state.json` at mode 0600, like `.netrc`. The local
  proxy relay listens on 127.0.0.1 without authentication of its own.
- **Page content is third-party input.** `read`, `snap`, `console` and `net`
  print what websites put there, unsanitized. An agent reading it should treat
  it as untrusted (prompt injection).
