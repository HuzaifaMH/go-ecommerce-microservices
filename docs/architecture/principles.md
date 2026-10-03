# Design principles

| Principle | How it shows up | Why | Decision |
|---|---|---|---|
| Typed contracts | gRPC + Protobuf inside, REST/JSON at the edge | Generated code, deadlines, breaking-change checks with `buf` | [ADR-0002](../adr/0002-grpc-internal-rest-edge.md) |
| Durable messaging | NATS JetStream with durable pull consumers | Lightweight, built-in de-duplication, simple to operate | [ADR-0003](../adr/0003-nats-jetstream-over-kafka.md) |
| No distributed transactions | Orchestrated saga with compensation | Each service commits locally; failures are undone by explicit steps | [ADR-0004](../adr/0004-orchestrated-saga.md), [ADR-0013](../adr/0013-saga-failure-handling.md) |
| No lost or duplicate events | Transactional outbox, idempotent inbox | State change and message commit together; redelivery is harmless | [ADR-0005](../adr/0005-transactional-outbox-and-idempotent-consumers.md) |
| Clear data ownership | PostgreSQL per service, `sqlc` queries, `goose` migrations | No shared tables; type-safe SQL | [ADR-0006](../adr/0006-database-per-service-postgres.md), [ADR-0011](../adr/0011-sqlc-goose-and-startup-migrations.md) |
| Enforced boundaries | `internal/` per service, layer test in `test/architecture` | The compiler and CI reject coupling | [ADR-0007](../adr/0007-service-code-structure.md) |
| Secure by default | RS256-only JWT (algorithm pinned), ownership checks, rate limits | Closes common token attacks; IDs cannot be probed | [ADR-0015](../adr/0015-gateway-edge-security.md) |
| Observable | One order is one trace; RED and business metrics; tested alerts | "What happened to order X?" is answerable in seconds | [ADR-0009](../adr/0009-observability.md), [ADR-0016](../adr/0016-observability.md) |
| Resilient start-up | Retry with backoff for Postgres and NATS | Container start order does not matter | [CHANGELOG](../../CHANGELOG.md) |
| Tested at every level | Unit, integration, architecture, end-to-end, alert rules | Confidence to change behaviour | [ADR-0010](../adr/0010-testing-strategy.md) |
