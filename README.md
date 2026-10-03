# Go E-Commerce Microservices

[![CI](https://github.com/HuzaifaMH/go-ecommerce-microservices/actions/workflows/ci.yml/badge.svg)](https://github.com/HuzaifaMH/go-ecommerce-microservices/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![gRPC](https://img.shields.io/badge/gRPC-Protobuf-244c5a)
![NATS](https://img.shields.io/badge/NATS-JetStream-27AAE1)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![OpenTelemetry](https://img.shields.io/badge/OpenTelemetry-traces-425cc7)
![License](https://img.shields.io/badge/license-MIT-green)

An event-driven e-commerce backend in Go. Five services cooperate over **gRPC** and **NATS JetStream** to place, pay for and fulfil an order, using an **orchestrated saga** with compensation, the **transactional outbox** pattern and **idempotent consumers**. Every design choice is recorded as an [Architecture Decision Record](docs/adr/README.md).

![Architecture](docs/architecture/containers.svg)

| Service | Responsibility |
|---|---|
| [`api-gateway`](services/gateway/README.md) | Public REST edge: JWT auth, rate limiting, translates to gRPC |
| [`order-service`](services/order/README.md) | Order lifecycle and saga orchestrator |
| [`inventory-service`](services/inventory/README.md) | Stock levels, reservations and releases |
| [`payment-service`](services/payment/README.md) | Charges and refunds through a pluggable (simulated) provider |
| [`notification-service`](services/notification/README.md) | Email/SMS notifications from domain events |

## Quick start

Requires Docker.

```bash
make up    # NATS, Postgres, 5 services, Jaeger, Prometheus, Grafana
```

```bash
TOKEN=$(curl -s -X POST localhost:8080/dev/token -H 'Content-Type: application/json' -d '{"subject":"alice"}' | jq -r .access_token)
curl -s -X POST localhost:8080/v1/orders -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: demo-1' \
     -H 'Content-Type: application/json' -d '{"items":[{"sku":"BOOK-GO-001","quantity":1}]}'
```

Then open the traces at http://localhost:16686 and the dashboard at http://localhost:3000. Stop with `make down`.

## Documentation

[Architecture](docs/architecture/README.md) · [Design principles](docs/architecture/principles.md) · [Decision records](docs/adr/README.md) · [Observability](docs/observability.md) · [Development](docs/development.md) · [Security](SECURITY.md) · [Changelog](CHANGELOG.md)

## License

[MIT](LICENSE)
