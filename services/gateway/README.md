# api-gateway

Public REST/JSON edge: JWT auth, rate limiting, request IDs. Translates REST calls to gRPC. Owns no data.

| Path | Purpose |
|---|---|
| `cmd/gateway/` | Entrypoint; wiring only |
| `internal/http/` | Router, handlers, middleware |
| `internal/clients/` | gRPC clients for order and inventory |
| `internal/config/` | Environment-based configuration |
