# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private reporting instead: **Security → Report a vulnerability** on this repository. You will get a reply within a few days.

## Scope and design notes

This is a portfolio-grade reference system. Its security design is documented so reviewers can judge it:

- The gateway accepts **RS256 JWTs only**; the algorithm is pinned, so `alg: none` and HMAC confusion attacks are rejected.
- Keys come from a PEM file or a JWKS URL (cached, refreshed on rotation). The `/dev/token` issuer exists only when `DEV_AUTH=true` and must never be enabled in production.
- Orders are scoped to their owner; another customer's order returns `404`, not `403`, so IDs cannot be probed.
- Requests are rate limited per caller. Internal gRPC ports and `/metrics` are not meant to be exposed publicly.
- Payment and notification providers are simulated; no real card or contact data is processed.
- Dependencies are kept current with Dependabot.
