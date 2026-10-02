# ADR-0001: Microservices vs modular monolith

**Status:** Accepted

## Context
The project exists to demonstrate distributed-systems design in Go: service boundaries, asynchronous messaging, failure handling and observability.

## Decision
Build separate services (gateway, order, inventory, payment, notification) in one repository.

## Alternatives considered
Modular monolith: simpler to run and the right default for a small team and a product at this size. Rejected here because it would not exercise the distributed concerns this project is meant to show.

## Consequences
Operational overhead and distributed failure modes (partial failure, duplicates, eventual consistency) are accepted and handled explicitly by ADR-004 and ADR-005. The README states that a real team at this scale would likely start as a modular monolith.
