# ADR-0004: Orchestrated saga for order processing

**Status:** Accepted

## Context
Placing an order spans inventory and payment, each with its own database, so a distributed transaction is not available.

## Decision
order-service orchestrates the saga as an explicit state machine (PENDING, STOCK_RESERVED, CONFIRMED, CANCELLED) and issues commands; inventory and payment only handle commands and reply. Notification consumes resulting events.

## Alternatives considered
Choreography: looser coupling, but the end-to-end flow and compensation logic are spread across services and hard to trace and test.

## Consequences
The workflow and its compensations live in one place and are easy to test. order-service becomes a coupling point that knows the steps.
