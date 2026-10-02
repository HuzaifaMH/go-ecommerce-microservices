# Go E-Commerce Microservices

[![CI](https://github.com/HuzaifaMH/go-ecommerce-microservices/actions/workflows/ci.yml/badge.svg)](https://github.com/HuzaifaMH/go-ecommerce-microservices/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![gRPC](https://img.shields.io/badge/gRPC-Protobuf-244c5a)
![NATS](https://img.shields.io/badge/NATS-JetStream-27AAE1)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-green)

An event-driven e-commerce backend built in Go. Order, inventory, payment and notification services communicate over **gRPC** and **NATS JetStream**, using an **orchestrated saga**, the **transactional outbox** pattern and idempotent consumers.

> **Status:** under active development. See the [roadmap](#roadmap).

## Architecture

```mermaid
flowchart LR
    Client([Client]) -- REST/JSON --> GW[api-gateway]
    GW -- gRPC --> ORD[order-service]
    GW -- gRPC --> INV[inventory-service]
    ORD <-- "commands / replies<br/>(JetStream)" --> INV
    ORD <-- "commands / replies<br/>(JetStream)" --> PAY[payment-service]
    ORD -- "order events<br/>(JetStream)" --> NOT[notification-service]
    PAY -- "payment events<br/>(JetStream)" --> NOT
    ORD --- ODB[(orders DB)]
    INV --- IDB[(inventory DB)]
    PAY --- PDB[(payments DB)]
    NOT --- NDB[(notifications DB)]
```

| Service | Responsibility |
|---|---|
| `api-gateway` | Public REST edge, JWT auth, rate limiting, translates to gRPC |
| `order-service` | Order lifecycle and saga orchestrator |
| `inventory-service` | Stock levels, reservations and releases |
| `payment-service` | Charges and refunds via a pluggable (simulated) provider |
| `notification-service` | Email/SMS notifications from domain events |

### Order flow

1. Client creates an order; it is stored as `PENDING` together with an outbox message.
2. Inventory reserves stock, then payment charges the customer.
3. On success the order becomes `CONFIRMED`. If payment fails, stock is released and the order is `CANCELLED`.
4. Notification reacts to the resulting events.

Full details: [docs/architecture.md](docs/architecture.md).

## Key design decisions

| Decision | Choice | Why |
|---|---|---|
| Internal APIs | gRPC + Protobuf | Typed contracts, deadlines, generated code |
| Public API | REST via gateway | Client compatibility |
| Messaging | NATS JetStream | Lightweight, durable, built-in de-duplication |
| Consistency | Orchestrated saga + outbox/inbox | No distributed transactions; no lost or duplicate events |
| Data | PostgreSQL per service, pgx + sqlc | Clear ownership, type-safe SQL |

Every decision, with alternatives and consequences, is recorded in [docs/adr](docs/adr/README.md).

## Getting started

Requirements: Go 1.26+, Docker.

```bash
make tools      # install pinned buf, protoc plugins and sqlc into ./bin
make proto      # regenerate gen/ from api/proto
make sqlc       # regenerate typed query code
make up         # start NATS JetStream and PostgreSQL
make test       # run all tests with the race detector (integration tests need Docker)
make test-short # fast tests only
make down       # stop everything
```

Run `make help` for all targets.

## Repository layout

```
api/proto/            Protobuf contracts (buf)
gen/                  Generated Go code
services/<service>/   One self-contained directory per service
  cmd/<service>/        entrypoint (wiring only)
  internal/domain/      entities and business rules
  internal/app/         use cases and ports
  internal/adapters/    gRPC, JetStream, Postgres implementations
  migrations/           SQL migrations
pkg/                  shared, service-agnostic libraries
deploy/               Docker Compose, Kubernetes manifests
test/                 architecture and end-to-end tests
docs/                 architecture and ADRs
```

Service boundaries are enforced by the Go compiler (`internal/`); layer boundaries inside a service are enforced by `test/architecture`. See [ADR-0007](docs/adr/0007-service-code-structure.md).
## Roadmap

- [x] Phase 0 — repository scaffold, CI, local infrastructure, ADRs
- [x] Phase 1 — protobuf contracts and shared packages (config, logging, health, runner, messaging, outbox/inbox)
- [x] Phase 2 — [inventory-service](services/inventory/README.md)
- [ ] Phase 3 — payment-service
- [ ] Phase 4 — order-service and saga
- [ ] Phase 5 — notification-service
- [ ] Phase 6 — api-gateway
- [ ] Phase 7 — observability (OpenTelemetry, Prometheus, Grafana, Jaeger)
- [ ] Phase 8 — Kubernetes manifests and release pipeline
- [ ] Phase 9 — Angular admin UI

## License

[MIT](LICENSE)
