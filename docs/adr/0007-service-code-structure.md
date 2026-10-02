# ADR-0007: Pragmatic hexagonal structure in a single Go module

**Status:** Accepted

## Context
We want testable business logic, clear boundaries, and low ceremony.

## Decision
One Go module. Per service: `cmd/<service>` for wiring, `internal/<service>/{domain,app,adapters,config}`. Shared infrastructure in `pkg/`. Interfaces are defined where they are consumed. A `depguard` rule forbids services importing each other.

## Alternatives considered
Multi-module or go.work: independent versioning, but heavy for this size. Flat package layout: faster initially, harder to keep boundaries.

## Consequences
Easy refactoring and a single `go test ./...`. Boundaries are enforced by lint rather than the module system.
