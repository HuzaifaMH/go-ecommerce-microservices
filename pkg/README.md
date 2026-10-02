Shared, service-agnostic libraries. Nothing here may depend on a specific service.

| Package | Purpose |
|---|---|
| `config` | Typed environment configuration that reports every invalid setting at once |
| `logging` | `log/slog` logger (JSON or text) tagged with the service name |
| `health` | `/healthz` liveness and `/readyz` readiness endpoints with pluggable checks |
| `runner` | Runs a service's tasks together; graceful HTTP and gRPC shutdown on SIGINT/SIGTERM |
| `messaging` | NATS JetStream publisher (de-duplicates by message ID), durable consumer with ack/retry/terminate semantics, idempotent-handler middleware |
| `outbox` | Relay that publishes pending outbox rows to the broker |
| `outbox/pgstore` | PostgreSQL outbox and inbox: `WithTx`, `Enqueue`, `Dispatch` (`FOR UPDATE SKIP LOCKED`), `Once` |
| `version` | Build metadata injected with `-ldflags` |