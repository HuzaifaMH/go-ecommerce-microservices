# ADR-0014: Notification delivery is at-least-once, keyed by (order, kind, channel)

**Status:** Accepted

## Context
Order events arrive at-least-once and can be duplicated. The notification service must not spam customers with repeats, and must not lose a notification either. Sending is an external call, so it cannot be part of a database transaction (ADR-0012).

## Decision
- **Record first, then send.** Each notification is inserted under a `UNIQUE (order_id, kind, channel)` (`ON CONFLICT DO NOTHING`) before anything is sent. A repeated event finds the existing row instead of creating another.
- **Send only what is pending**, then mark it `sent`, or `failed` if the sender reports the message permanently undeliverable. Final states never change (the update statements only touch `pending` rows).
- **Transient send failure:** the error is returned, the broker redelivers the event, and only the notifications still pending are tried again. A failing channel does not hold back the others.
- **No inbox and no outbox.** The unique key is the idempotency record, and this service publishes no events.
- **Recipient:** the customer ID is used as the email address and phone number, because order events carry no contact details.

## Alternatives considered
- **Inbox around the whole handler:** keeps a transaction open while the sender runs, and records the *event* as done even if one channel failed.
- **At-most-once (mark sent before sending):** never duplicates, but a crash silently loses a notification the customer needed.
- **Claim with a lease (`pending -> sending`) to get closer to exactly-once:** extra states and a recovery path for abandoned claims. Not worth it for notifications; revisit if duplicates become a real problem.
- **Subscribing to `payment.evt.failed`:** it carries no customer ID, and the order service already publishes `order.evt.cancelled` with the reason.

## Consequences
- A message is never lost, and repeated events never produce repeats in normal operation.
- In two narrow cases a message can be sent twice: the process dies between sending and marking sent, or two instances process the same event at the same instant. The integration tests pin this down: sequential repeats send once, and a concurrent race converges to one row per notification with every message delivered at least once.
- Notifications still pending when the broker gives up (five failed deliveries) stay `pending`; a reaper that retries or fails them is future work.
- Replacing the simulated sender with a real provider, or the customer ID with a looked-up contact, touches only adapters.
