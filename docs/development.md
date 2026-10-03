# Development

Requirements: Go 1.26+, Docker. Contribution workflow and commit conventions are in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Common tasks

```bash
make tools      # install pinned buf, protoc plugins and sqlc into ./bin
make proto      # regenerate gen/ from api/proto
make sqlc       # regenerate typed query code
make up         # build and start the whole stack
make down       # stop it
make test       # all tests with the race detector (integration tests need Docker)
make test-short # fast tests only
make test-race-docker  # short tests under the race detector in a Linux container (for Windows)
make e2e        # build the stack and run end-to-end tests against it
make rules-test # check the Prometheus config and unit-test the alert rules
make lint       # golangci-lint
```

Run `make help` for all targets.

## Repository layout

```text
api/proto/            Protobuf contracts (buf)
gen/                  Generated Go code
services/<service>/   One self-contained directory per service
  cmd/<service>/        entrypoint (wiring only)
  internal/domain/      entities and business rules
  internal/app/         use cases and ports
  internal/adapters/    gRPC, JetStream, Postgres implementations
  migrations/           SQL migrations
pkg/                  Shared, service-agnostic libraries
deploy/               Docker Compose, Dockerfile, Prometheus and Grafana config
test/                 Architecture and end-to-end tests
docs/                 This documentation and the ADRs
```

Service boundaries are enforced by the Go compiler (`internal/`); layer boundaries inside a service by `test/architecture`. See [ADR-0007](adr/0007-service-code-structure.md).

## Tests

| Kind | Where | Needs Docker |
|---|---|---|
| Unit | next to the code | no |
| Integration (testcontainers: Postgres, NATS) | `services/*/internal/integration` | yes |
| Architecture (layer rules) | `test/architecture` | no |
| End to end (full compose stack) | `test/e2e` | yes |
| Alert rules (`promtool`) | `deploy/prometheus/rules_test.yml` | yes |

Strategy and trade-offs: [ADR-0010](adr/0010-testing-strategy.md).

## Continuous integration

GitHub Actions runs 12 required checks on every pull request: lint, tests with `-race`, `buf` (lint and breaking changes), `sqlc`, Compose validation, Prometheus rule tests, an image build per service, and the end-to-end suite. `main` is protected; changes land by squash-merged pull request with a Conventional Commit title.
