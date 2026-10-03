# ADR-0007: Service-per-directory monorepo with clean architecture layers

**Status:** Accepted (supersedes the earlier flat `cmd/` + `internal/<service>` layout)

## Context
We want testable business logic, clear service boundaries, and a layout that scales to more services and teams. Large Go monorepos typically give each service its own self-contained directory and keep shared code small.

## Decision
One repository, one Go module, one directory per service:

```
.
├── api/proto/                 # Protobuf contracts (buf), shared by all services
├── gen/                       # Generated Go code (committed)
├── services/
│   └── <service>/
│       ├── cmd/<service>/     # main.go: wiring only
│       ├── internal/
│       │   ├── domain/        # entities, value objects, business rules
│       │   ├── app/           # use cases + ports (interfaces)
│       │   ├── adapters/      # grpc, nats, postgres implementations
│       │   └── config/
│       ├── migrations/        # SQL (goose)
│       └── Dockerfile
├── pkg/                       # shared, service-agnostic libraries
├── deploy/                    # Compose, Dockerfile, monitoring config
├── test/                      # architecture and e2e tests
├── scripts/  tools/  docs/
```

Boundaries are enforced by tooling, not convention:
1. **Between services:** code lives under `services/<name>/internal/`, so the Go compiler rejects imports from any other service. Services talk over gRPC and JetStream only.
2. **Inside a service:** `test/architecture` fails CI if `domain` imports `app`, `adapters`, `config` or infrastructure libraries (gRPC, NATS, pgx, `net/http`), or if `app` imports `adapters`.
3. **Shared code** goes in `pkg/` and must not depend on any service.

## Mapping to Clean Architecture
| Clean Architecture ring | Here |
|---|---|
| Entities | `domain/` |
| Use cases | `app/` |
| Interface adapters and frameworks | `adapters/` (gRPC handlers, JetStream, Postgres) and `cmd/` for wiring |

Entities and use cases are separate packages; presenters and controllers are folded into `adapters/` to avoid boilerplate. Dependencies point inward: `adapters -> app -> domain`. The api-gateway has no domain layer; it contains `internal/http` and `internal/clients`.

## Alternatives considered
- **Flat `cmd/` + `internal/<service>` layout:** simple, but all services share one `internal/` tree, so boundaries need lint rules instead of compiler guarantees.
- **One `go.mod` per service (go.work):** independent versioning, but heavy for this size and complicates shared protos and CI.
- **Four separate layer packages (entities, usecases, interface adapters, frameworks):** closer to the textbook, more boilerplate for little gain.

## Consequences
Each service can be built, tested and containerised on its own, and the structure scales to new services. Shared contracts live in one place. Domain logic is testable without any infrastructure.