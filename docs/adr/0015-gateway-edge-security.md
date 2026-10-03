# ADR-0015: How the gateway authenticates and protects the API

**Status:** Accepted

## Context
The gateway is the only service reachable from outside. It must decide who a caller is, stop callers from touching each other's data, and survive abuse, without becoming an identity provider itself.

## Decision
**Verify tokens, never issue them.** Callers present RS256 JWT bearer tokens issued by an identity provider. The gateway holds only public keys: a PEM file, or a JWKS URL for providers that rotate keys. Only the `RS256` algorithm is accepted, set before parsing, which rules out `alg: none` and the attack that re-signs a token with HS256 using the public key as the secret. Issuer, audience, expiry (required) and subject are all checked.

**Identity comes from the token, nothing else.** The customer on an order is the token's `sub`. The request has no customer or price field and unknown JSON fields are rejected, so a caller cannot order as someone else or choose a price. Admins are recognised by a `roles` claim.

**Other people's data does not exist.** Fetching, cancelling or reading the notifications of another customer's order returns `404`, identical to an order that does not exist, so IDs cannot be probed. The ownership check happens in the gateway before the cancel command or the notification query is sent.

**Failures are classified.** A bad token is `401`. Being unable to obtain signing keys (identity provider down) is `503`, because it is our problem, not the caller's. Known keys keep working through an outage, and JWKS refreshes are rate limited so unknown key IDs cannot be used to hammer the provider.

**Rate limiting** is a token bucket per caller, keyed by subject when authenticated and by client address otherwise. Health probes are exempt.

**Development convenience, fenced off.** With `DEV_AUTH=true` the gateway generates a throwaway RSA key and exposes `POST /dev/token`. It logs a warning at start-up, cannot be combined with real key settings, and the route does not exist otherwise. Compose enables it; nothing else should.

**Small things that matter:** strict JSON, body size limit, server timeouts, request IDs (client-supplied ones are accepted only if they are safe to put in a log line), `no-store` and `nosniff`, panic recovery, backend error text never leaked for internal errors, tokens never logged, CORS only for configured origins, JWKS URLs must be HTTPS.

## Alternatives considered
- **Issue tokens in the gateway or a bundled auth service:** a whole subsystem (users, passwords, refresh, revocation) that is not the point of this project and best left to an identity provider.
- **HS256 with a shared secret:** simpler, but every service able to verify could also forge tokens, and the secret has to be distributed. RS256 lets the gateway verify with a public key.
- **Return `403` for other people's orders:** reveals that the order exists.
- **Distributed rate limiting (Redis):** accurate across instances but one more moving part. The in-memory limiter is enough to protect each instance.
- **Trust `X-Forwarded-For`:** correct only if you know exactly which proxies sit in front, so it is not done blindly.

## Consequences
- The gateway has no user database and no secrets to protect, only public keys.
- Tokens cannot be revoked before they expire; keep lifetimes short. There is no refresh flow here, which belongs to the identity provider.
- Rate limits are per gateway instance, so the effective limit is the configured rate times the number of instances.
- Behind a load balancer, unauthenticated callers are limited by the balancer's address until forwarded-address handling is configured for that deployment.
- Backends trust the gateway: the internal gRPC calls are not authenticated or encrypted here. In production that would be mutual TLS or a service mesh.
