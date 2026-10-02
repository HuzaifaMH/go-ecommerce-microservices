# ADR-0008: Go engineering practices

**Status:** Accepted

## Context
The repo should show idiomatic, production-grade Go.

## Decision
`context.Context` first parameter with deadlines everywhere; `errgroup` for lifecycle and graceful shutdown; errors wrapped with `%w` and checked with `errors.Is/As`; `log/slog` structured logging; configuration from environment variables validated at startup; bounded concurrency in workers; health and readiness endpoints; no global mutable state.

## Alternatives considered
Third-party frameworks for DI and logging: rejected in favour of the standard library where it is sufficient.

## Consequences
More explicit wiring code in `main.go`, in exchange for clarity and easy testing.
