# E-Commerce Microservices in Go — Architecture & Build Plan

Working repo name: `go-ecommerce-microservices` (draft, not yet created on GitHub)

## 1. Goals

1. Show Go's strengths: concurrency, small static binaries, fast startup, strong stdlib, simple deployment.
2. Show production-minded design: explicit failure handling, idempotency, observability, testing, CI/CD.
3. Stay small enough to finish and explain every decision in an interview.

**Non-goals:** a customer storefront, real payment processing, multi-region, a service mesh, cloud (AWS) deployment.

## 2. Services

| Service | Responsibility | Owns data | Sync API (gRPC) | Async (JetStream) |
|---|---|---|---|---|
| **api-gateway** | Public REST/JSON edge, JWT auth, rate limit, request-ID, translates to gRPC | none | calls order, inventory | none |
| **order-service** | Order lifecycle + saga orchestrator | `orders`, `order_items`, `saga_state`, `outbox` | `CreateOrder`, `GetOrder`, `ListOrders`, `CancelOrder` | publishes commands, consumes replies, emits order events |
| **inventory-service** | Stock levels, reservations, release | `products_stock`, `reservations`, `outbox`, `inbox` | `GetStock`, `ListStock` | handles `ReserveStock` / `ReleaseStock` |
| **payment-service** | Charges and refunds through a pluggable provider (simulated) | `payments`, `outbox`, `inbox` | `GetPayment` | handles `ChargePayment` / `RefundPayment` |
| **notification-service** | Email/SMS (logged/simulated) from domain events | `notifications`, `inbox` | none | consumes order/payment events |

A product catalog service is deliberately left out. Inventory carries SKU, name and price snapshot. It can be added later as a demo of extending the system.

## 3. Order flow (saga)

```
Client -> Gateway (REST) -> Order (gRPC CreateOrder)
  Order: insert order=PENDING + outbox(ReserveStock)   [one DB tx]
  Inventory: reserve stock  -> StockReserved | StockRejected
  Order: on StockReserved -> outbox(ChargePayment)
  Payment: charge          -> PaymentSucceeded | PaymentFailed
  Order: on PaymentSucceeded -> CONFIRMED, emit OrderConfirmed
         on PaymentFailed    -> outbox(ReleaseStock) -> CANCELLED, emit OrderCancelled
  Notification: consumes OrderConfirmed / OrderCancelled / PaymentFailed
```

Order states: `PENDING → STOCK_RESERVED → CONFIRMED`, or `→ CANCELLED` (with the compensation path).
`CreateOrder` returns `202`-style semantics: the order is accepted as `PENDING`. The client polls `GetOrder` (a streaming `WatchOrder` RPC can be added later).

## 4. Design decisions (ADR summaries)

Each becomes a file in `docs/adr/` using Context / Decision / Alternatives / Consequences.

### ADR-001 Microservices vs modular monolith
- **Decision:** microservices.
- **Why:** the project exists to demonstrate distributed-systems design. In a real company at this size I would start with a modular monolith. The README will say so, which shows judgement.
- **Cost accepted:** operational overhead, distributed failure modes.

### ADR-002 gRPC internally, REST at the edge
- **gRPC for service to service:** a typed contract in protobuf, generated clients and servers, HTTP/2, deadlines and cancellation propagation, streaming, backward-compatibility checks with `buf breaking`.
- **REST/JSON for external clients:** browsers, curl and third parties expect it. The gateway keeps that concern out of the services.
- **Alternatives:** REST everywhere (loses the contract and generated code), GraphQL (unneeded complexity here), grpc-gateway (considered; a hand-written gateway on the standard library's net/http router is simpler to read and to test).
- **Cost:** two API styles to maintain. The gateway is thin to keep that small.

### ADR-003 NATS JetStream vs Kafka vs RabbitMQ
- **Decision:** JetStream.
- **Why:**
  - One small Go binary, so it is trivial to run in a container.
  - Subject-based routing and wildcards fit command and event subjects (`inventory.cmd.reserve`, `order.evt.confirmed`).
  - Durable pull consumers, explicit ack, redelivery, `MaxDeliver` and dead-letter handling via advisories.
  - Built-in publish de-duplication with `Nats-Msg-Id` (needed for the outbox).
  - A first-class Go client written by the same ecosystem.
- **Kafka is better when:** you need very high throughput, long retention with replay by offset, and its connector and stream-processing ecosystem. None of those are requirements here.
- **RabbitMQ:** strong routing, but weaker replay and a less natural fit for streams.
- **Cost:** a smaller ecosystem and fewer people know it. The README will state when I would pick Kafka.

### ADR-004 Orchestrated saga, not choreography
- **Decision:** order-service orchestrates; other services only react to commands and reply.
- **Why:** the workflow is a single explicit state machine in one place. That is easy to test, trace and explain, and compensation logic is not scattered across services.
- **Choreography** gives looser coupling but makes "what happens to an order?" hard to answer. Notification still uses choreography by consuming events.
- **Cost:** order-service knows the steps, which makes it a coupling point.

### ADR-005 Transactional outbox + idempotent consumers
- **Problem:** "write to DB, then publish" can lose or duplicate events if the process crashes between the two.
- **Decision:** write state and an `outbox` row in one transaction. A relay goroutine publishes unsent rows, using the row ID as `Nats-Msg-Id` so JetStream de-duplicates. Consumers record processed message IDs in an `inbox` table, so processing is effectively exactly-once.
- **Guarantee:** at-least-once delivery plus idempotent handling.

### ADR-006 Database per service (PostgreSQL)
- **Decision:** each service owns its schema, with no cross-service queries. In local Compose it is one Postgres container with separate databases and credentials per service. Production would use separate instances.
- **Access:** `pgx` plus `sqlc` (type-safe SQL, no ORM magic). Migrations with `goose`.
- **Redis:** optional, for gateway rate limiting only. Added only if used.

### ADR-007 Service-per-directory layout with clean architecture layers
Each service lives in `services/<name>/` with `cmd/`, `internal/{domain,app,adapters,config}`, `migrations/`. Shared code is in `pkg/`, contracts in `api/proto`. The Go `internal/` rule blocks cross-service imports at compile time, and `test/architecture` enforces `adapters -> app -> domain` inside each service. Full detail in `docs/adr/0007-service-code-structure.md`.

### ADR-008 Go practices to showcase
`context` everywhere with deadlines, `errgroup` for lifecycle and graceful shutdown, wrapped errors with `errors.Is/As`, `log/slog` structured logging, table-driven tests, `-race` in CI, worker pools or bounded concurrency in the outbox relay, config from environment variables with validation at startup, health and readiness endpoints.

### ADR-009 Observability
OpenTelemetry traces and metrics with the trace context propagated through gRPC metadata and NATS headers, so one order is one trace across all services. Prometheus + Grafana for metrics, Jaeger for traces. `slog` JSON logs carry `trace_id`.

### ADR-010 Testing strategy
- Unit tests for domain and app layers (fast, no I/O).
- Integration tests with `testcontainers-go` (real Postgres and NATS).
- One end-to-end test that runs the full saga against Compose: happy path, payment failure with compensation, and duplicate message delivery.
- `buf lint` and `buf breaking` on protos.

## 5. Deployment and running

| Environment | How | Purpose |
|---|---|---|
| **Local dev** | `docker compose up` brings up NATS, Postgres, all services, Prometheus, Grafana and Jaeger. `make` targets for everything else. | One command to try it |
| **CI** | GitHub Actions: lint (`golangci-lint`), `buf`, test with `-race`, build multi-stage distroless images, push to GHCR on tags | Shows automation |

Images: multi-stage builds, `CGO_ENABLED=0`, distroless non-root, expected size around 15–25 MB per service.

## 6. Repository standards
Conventional Commits, branch protection, PR template, CODEOWNERS, `Makefile`, `.golangci.yml`, `.editorconfig`, MIT license, README with an architecture diagram, `CONTRIBUTING.md`, issues and milestones mapped to the phases below, Dependabot, release tags.

## 7. Build phases (each ends in a working, tested, committed state)

| Phase | Deliverable |
|---|---|
| 0 | Repo scaffold: module, Makefile, lint config, CI skeleton, Compose with NATS and Postgres, ADRs |
| 1 | Protos and `pkg/` basics (logging, config, health, graceful shutdown, `natsx` with outbox/inbox) |
| 2 | inventory-service end to end (gRPC, Postgres, reserve/release handlers) with tests |
| 3 | payment-service with a simulated provider and idempotency keys |
| 4 | order-service with the saga state machine and compensation |
| 5 | notification-service |
| 6 | api-gateway (REST, JWT, rate limit) |
| 7 | Observability stack and dashboards |
| 8 | Release v0.1.0 (Kubernetes manifests were deferred and are not part of this release) |
| 9 | Angular admin UI (orders, stock, live order status) served behind the gateway; added after the backend is complete |

## 8. Decisions confirmed
1. Angular UI is included, as Phase 9, after the backend is done.
2. No AWS phase. The deployment target is Docker Compose; Kubernetes manifests were deferred.
3. License: MIT.
