# api-gateway

The public front door. It exposes a REST/JSON API, authenticates callers with RS256 JWT bearer tokens, rate limits them, and translates their requests into gRPC calls to the order, inventory and notification services. It owns no data.

## Endpoints

| Method and path | Auth | Description |
|---|---|---|
| `GET /v1/products`, `GET /v1/products/{sku}` | none | The catalogue (name, price, availability) |
| `POST /v1/orders` | token | Place an order. Requires an `Idempotency-Key` header. Returns `202 Accepted` with a `Location`; the outcome arrives asynchronously, poll the order |
| `GET /v1/orders` | token | Your orders, newest first (`page_size`, `page_token`). Admins may pass `customer_id` or list everyone's |
| `GET /v1/orders/{id}` | token | One order |
| `POST /v1/orders/{id}/cancel` | token | Cancel before confirmation (`409` once confirmed). Optional body `{"reason": "..."}` |
| `GET /v1/orders/{id}/notifications` | token | What the customer was told about the order |
| `GET /healthz`, `GET /readyz` | none | Liveness, and readiness including the three backends |
| `POST /dev/token` | none | **Development only** (`DEV_AUTH=true`): mints a token |

Request body for `POST /v1/orders`:

```json
{ "items": [ { "sku": "BOOK-GO-001", "quantity": 2 } ] }
```

There is deliberately **no `customer_id` and no price** in the request: unknown fields are rejected. The customer is always the token's subject, and the order is priced from the catalogue.

## Security model

- **RS256 only.** The gateway verifies tokens; it never issues them (except the dev endpoint). Tokens must be signed with RS256 by a trusted key and carry the expected `iss` and `aud`, an `exp` that has not passed (30 s clock-skew leeway) and a `sub`. Pinning the algorithm up front defeats `alg: none` and the HS256-with-the-public-key confusion attack; both are tested, including against the running gateway.
- **Keys:** a PEM public key file (`JWT_PUBLIC_KEY_FILE`) or a JWKS URL (`JWT_JWKS_URL`, HTTPS only). JWKS keys are cached, refreshed when a token names an unknown `kid` (key rotation, at most once per 30 s), and known keys keep working if the identity provider is briefly down. If keys cannot be obtained at all the answer is `503`, not `401`.
- **Ownership.** Orders belong to their customer. Someone else's order looks exactly like a missing one (`404`), so IDs cannot be probed. Admins (`roles` contains `admin`) can see everything.
- **Rate limiting** per caller (token subject, or client address when unauthenticated), as a token bucket. Over the limit: `429` with `Retry-After`. Health probes are exempt. State is per instance.
- **Safe defaults:** bodies limited to 1 MiB, strict JSON, server timeouts, `Cache-Control: no-store`, `nosniff`, panics become `500`, backend error details are never leaked, tokens are never logged.
- **CORS** is off unless `CORS_ALLOWED_ORIGINS` lists origins (for the browser admin UI).
- See [ADR-0015](../../docs/adr/0015-gateway-edge-security.md) for the reasoning and known limitations.

## Errors

Every error has the same shape and carries the request ID:

```json
{ "error": { "code": "invalid_argument", "message": "unknown SKU NOPE", "request_id": "..." } }
```

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_argument` | Bad input |
| 401 | `unauthenticated` | Missing, invalid or expired token (`WWW-Authenticate` is set) |
| 403 | `permission_denied` | Listing another customer's orders |
| 404 | `not_found` | Unknown endpoint, product or order (including other people's) |
| 409 | `already_exists` / `failed_precondition` | Idempotency key reused for a different request / order can no longer be cancelled |
| 413 / 415 | `payload_too_large` / `unsupported_media_type` | Body too big / not JSON |
| 429 | `rate_limited` | Too many requests |
| 503 | `unavailable` | A backend or the identity provider is unavailable; retry |
| 504 | `timeout` | A backend took too long |

`X-Request-Id` is accepted from the client (if it is safe), generated otherwise, returned on every response, written to the logs, and forwarded to the backends as gRPC metadata.

## Try it

```bash
make up                                              # whole stack; the API is on http://localhost:8080
TOKEN=$(curl -s -X POST localhost:8080/dev/token -H 'Content-Type: application/json' \
          -d '{"subject":"alice"}' | jq -r .access_token)
curl -s localhost:8080/v1/products | jq
curl -si -X POST localhost:8080/v1/orders -H "Authorization: Bearer $TOKEN" \
     -H 'Idempotency-Key: demo-1' -H 'Content-Type: application/json' \
     -d '{"items":[{"sku":"BOOK-GO-001","quantity":1}]}'
curl -s localhost:8080/v1/orders -H "Authorization: Bearer $TOKEN" | jq
```

Use a customer name starting with `decline-` to see a declined payment, and `bounce-` for an undeliverable notification. For an admin token send `"roles":["admin"]`.

## Layout

```
cmd/gateway/             wiring only
internal/api/            routing, middleware, handlers, JSON contract, error mapping
internal/auth/           RS256 verification, PEM and JWKS key sources, dev token issuer
internal/ratelimit/      token-bucket limiter per caller
internal/clients/        gRPC connections with default deadlines and request-ID propagation
internal/requestid/      request ID helpers
internal/config/         environment configuration with strict validation
```

## Configuration (environment)

| Variable | Default | Description |
|---|---|---|
| `JWT_ISSUER`, `JWT_AUDIENCE` | *required* (unless `DEV_AUTH`) | Expected `iss` and `aud` of tokens |
| `JWT_JWKS_URL` | | JWKS endpoint of the identity provider (HTTPS; HTTP only for localhost) |
| `JWT_PUBLIC_KEY_FILE` | | PEM public key; use this **or** the JWKS URL |
| `DEV_AUTH` | `false` | Generate a throwaway key pair and enable `POST /dev/token`. **Never in production**; it cannot be combined with the JWT key settings |
| `ORDER_GRPC_ADDR`, `INVENTORY_GRPC_ADDR`, `NOTIFICATION_GRPC_ADDR` | `localhost:9092`, `:9090`, `:9093` | Backend gRPC addresses |
| `BACKEND_TIMEOUT` | `5s` | Deadline for each backend call |
| `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST` | `20`, `40` | Per-caller limit; `RATE_LIMIT_RPS=0` disables |
| `CORS_ALLOWED_ORIGINS` | | Comma-separated origins (or `*`) |
| `MAX_BODY_BYTES` | `1048576` | Largest accepted request body |
| `HTTP_ADDR` | `:8080` | Listen address (public) |
| `METRICS_ADDR` | `:9100` | Internal address serving `/metrics`; must differ from `HTTP_ADDR` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | OTLP/gRPC endpoint for traces (e.g. `http://jaeger:4317`); unset disables export |
| `OTEL_TRACES_SAMPLE_RATIO` | `1` | Fraction of new traces recorded |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful shutdown limit |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | `debug\|info\|warn\|error`, `json\|text` |

## Test it

```bash
go test ./services/gateway/...    # unit tests, no Docker (auth attacks, ownership, rate limiting, CORS, ...)
make e2e                          # the whole system through the REST API
```
