# Contributing

## Workflow
1. Open an issue (or pick one) and link it in your PR.
2. Branch from `main`: `feat/<topic>`, `fix/<topic>`, `docs/<topic>`.
3. Keep PRs small and focused. Update or add an ADR in `docs/adr/` when a design decision changes.
4. `make test` and `make lint` must pass. CI runs both. After editing `api/proto` run `make proto`, and after editing SQL queries or migrations run `make sqlc`; commit the generated code (CI fails if it is stale).

## Commit messages
[Conventional Commits](https://www.conventionalcommits.org/): `feat(order): add saga state machine`, `fix(inventory): release stock on cancel`, `docs: ...`, `test: ...`, `chore: ...`.

## Code style
- `gofmt` / `goimports`, enforced by `golangci-lint`.
- Services live under `services/<name>/internal` and talk over gRPC or JetStream only. Inside a service the dependency rule is `adapters -> app -> domain`; `test/architecture` enforces it.
- Pass `context.Context` as the first parameter; wrap errors with `%w`; no global mutable state.
- Table-driven tests; run with `-race`.
