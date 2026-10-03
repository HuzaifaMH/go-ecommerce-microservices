# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-10-03

First release: a complete, runnable backend with `docker compose up`.

### Added
- **Contracts and shared packages**: Protobuf contracts managed with `buf`; shared config, logging, health, graceful-shutdown runner, JetStream messaging, and the transactional outbox and idempotent inbox.
- **inventory-service**: stock levels, reservations and releases over gRPC and JetStream.
- **payment-service**: charges and refunds through a pluggable simulated provider, protected by idempotency keys.
- **order-service**: order lifecycle and the orchestrated saga with compensation (stock is released when payment fails); orders can be cancelled while `PENDING` or `STOCK_RESERVED`.
- **notification-service**: at-least-once notifications keyed by (order, kind, channel), so a repeated event never notifies twice.
- **api-gateway**: REST/JSON edge with RS256-only JWT verification (PEM or JWKS with caching and rotation), per-caller rate limiting, CORS, request IDs, idempotency keys and ownership checks.
- **Observability**: OpenTelemetry traces that follow one order across all five services (trace context travels in outbox headers), Prometheus metrics, a Grafana dashboard, Jaeger, and eight alert rules unit-tested with `promtool`.
- **Quality gates**: 12 required CI checks, a layer-boundary architecture test, 26 end-to-end scenarios, 16 Architecture Decision Records.

### Fixed
- Services no longer crash when PostgreSQL or NATS is not ready at start-up; connections are retried with exponential backoff for up to 30 seconds, while a malformed configuration still fails immediately.

### Not included
- Kubernetes manifests and an Angular admin UI are not part of this release.

[0.1.0]: https://github.com/HuzaifaMH/go-ecommerce-microservices/releases/tag/v0.1.0
