# ADR-0005: Transactional outbox and idempotent consumers

**Status:** Accepted

## Context
Writing to the database and then publishing a message can lose or duplicate events if the process crashes between the two.

## Decision
Write state and an `outbox` row in a single transaction. A relay publishes unsent rows using the row ID as `Nats-Msg-Id`, so JetStream de-duplicates. Consumers record processed message IDs in an `inbox` table inside the same transaction as their side effects.

## Alternatives considered
Dual writes (unsafe), change-data-capture with Debezium (extra infrastructure), publishing before commit (can announce state that never persisted).

## Consequences
At-least-once delivery combined with idempotent handling gives effectively-once processing. Costs: extra tables and a relay goroutine per service.
