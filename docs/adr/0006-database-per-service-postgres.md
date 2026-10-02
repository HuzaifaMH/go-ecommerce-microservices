# ADR-0006: Database per service on PostgreSQL

**Status:** Accepted

## Context
Services must be able to change their schema and scale independently without coupling through shared tables.

## Decision
Each service owns its schema and role. Locally one Postgres container hosts separate databases; production would use separate instances. Access with pgx and sqlc, migrations with goose.

## Alternatives considered
Shared database (couples services), ORM (hides SQL and generated queries are harder to review), NoSQL per service (no requirement).

## Consequences
No cross-service joins; data is shared through APIs and events. sqlc gives compile-time-checked queries.
