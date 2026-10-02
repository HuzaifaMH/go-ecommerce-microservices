# ADR-0012: Call the payment provider outside the database transaction; no inbox for charges

**Status:** Accepted

## Context
Charging involves a slow, unreliable network call to an external provider and a database write that must be atomic with the event announcing the result. The inventory service wraps every command in an inbox transaction (ADR-0005). Doing that here would keep a database transaction, and its connection, open for the whole provider call.

## Decision
- Charge in three steps: (1) look up the order's existing payment, (2) call the provider **outside any transaction**, (3) in one short transaction insert the payment and enqueue the outcome event.
- Idempotency comes from the data, not an inbox: `payments.order_id` is `UNIQUE` and the insert is `ON CONFLICT DO NOTHING`. When another attempt wins the race, the loser publishes the winner's stored outcome.
- The provider is given the order ID as its idempotency key and must return the same result for the same order. A crash between steps 2 and 3 is repaired by redelivery: the provider is called again and answers identically.
- A decline is recorded as a failed payment and reported; it is never retried. An outage stores nothing and is retried by the broker.
- The payment consumer therefore does not use the inbox middleware.

## Alternatives considered
- **Inbox + provider call in one transaction:** simplest, but ties up a connection for as long as the provider takes, so a provider slowdown exhausts the connection pool and takes the service down.
- **Record "pending" first, then call the provider, then update:** also correct, but needs a recovery job for payments left pending by a crash and adds states and a second write.
- **Retry declines:** would charge a customer the provider already refused and make outcomes non-deterministic.

## Consequences
- Connections are held only for short writes.
- The design relies on the provider honouring idempotency keys, as real processors do. The simulated provider does the same.
- Duplicate commands produce duplicate (identical) reply events; the order-service must treat payment replies idempotently, which it needs to do anyway because delivery is at-least-once.
- If every redelivery fails, no reply is ever sent. The order-service sweeper cancels such orders after `SAGA_TIMEOUT` and releases their stock ([ADR-0013](0013-saga-failure-handling.md)).
