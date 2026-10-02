Shared, service-agnostic libraries. Nothing here may depend on a specific service.

| Package | Purpose |
|---|---|
| `config` | Typed environment configuration (strings, ints, floats, lists, durations) that reports every invalid setting at once |
| `logging` | `log/slog` logger (JSON or text) tagged with the service name |
| `health` | `/healthz` liveness and `/readyz` readiness endpoints with pluggable checks |
| `runner` | Runs a service's tasks together; graceful HTTP and gRPC shutdown on SIGINT/SIGTERM |
| `messaging` | NATS JetStream publisher (de-duplicates by message ID), durable consumer with ack/retry/terminate semantics, idempotent-handler middleware, protobuf message builder with correlation IDs |
| `outbox` | Relay that publishes pending outbox rows to the broker |
| `outbox/pgstore` | PostgreSQL outbox and inbox: `WithTx`, `Enqueue`, `Dispatch` (`FOR UPDATE SKIP LOCKED`), `Once` |
| `pagination` | Opaque keyset page tokens and page-size limits for newest-first lists |
| `platform` | Start-up plumbing: open Postgres, run embedded migrations, readiness checks, bind gRPC/HTTP listeners |
| `subjects` | JetStream stream and subject names shared between services (part of the contract) |
| `version` | Build metadata injected with `-ldflags` |