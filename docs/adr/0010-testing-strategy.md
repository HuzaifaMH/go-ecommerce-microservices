# ADR-0010: Testing strategy

**Status:** Accepted

## Context
We need fast feedback and confidence that the saga behaves correctly under failure.

## Decision
Unit tests for domain and application layers; integration tests with testcontainers-go against real Postgres and NATS; an end-to-end test of the full saga covering the happy path, payment failure with compensation, and duplicate delivery; `buf lint` and `buf breaking` on protos; `-race` in CI.

## Alternatives considered
Mocking the database and broker everywhere: faster, but hides real behaviour such as transactions and redelivery.

## Consequences
Integration tests need Docker, so they run in CI and are skipped with `-short` locally.
