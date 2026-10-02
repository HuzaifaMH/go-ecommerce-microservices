# order-service

Owns the order lifecycle and orchestrates the order saga (ADR-0004).

## Layout

| Path | Purpose |
|---|---|
| `cmd/order/` | Entrypoint; wiring only |
| `internal/domain/` | Entities, value objects, business rules. No I/O, no framework imports |
| `internal/app/` | Use cases; depends on small interfaces (ports) it defines |
| `internal/adapters/grpc/` | gRPC server (inbound) |
| `internal/adapters/nats/` | JetStream consumers and publishers |
| `internal/adapters/postgres/` | Repository and outbox/inbox implementations |
| `internal/config/` | Environment-based configuration |
| `migrations/` | SQL migrations (goose) |
