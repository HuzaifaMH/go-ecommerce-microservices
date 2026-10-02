# ADR-0013: How the order saga handles failure

**Status:** Accepted

## Context
ADR-0004 chose an orchestrated saga. Delivery is at-least-once, services can be slow or silent, and customers can cancel while replies are in flight. The design has to stay correct when messages are duplicated, delayed, reordered or lost.

## Decision
**One state machine, driven by replies.** `PENDING -> STOCK_RESERVED -> CONFIRMED`, or `CANCELLED` from `PENDING` / `STOCK_RESERVED`. `CONFIRMED` and `CANCELLED` are final. Every reply is applied to the order row under `SELECT ... FOR UPDATE`, in one transaction with the next command or event (outbox) and the inbox record.

**Duplicates and late replies are normal, not errors.** A reply that matches or follows the current state is ignored. Only a reply that *contradicts* the state (for example "payment succeeded" for a `PENDING` order) is an error, and it is terminated rather than retried.

**Compensation is always sent, and always idempotent.**
- Payment declined: cancel, send `ReleaseStock`, announce.
- Customer cancels, or the order times out: cancel, send `ReleaseStock`, announce. The release is sent even if the reservation has not been confirmed yet. Inventory processes commands in order, so it either undoes the reservation or finds nothing to do.
- A `StockReserved` that arrives for an already cancelled order triggers another `ReleaseStock`, so stock is never stranded by a race.
- Stock rejected: nothing was reserved, so there is nothing to release.

**Timeout.** A sweeper cancels unfinished orders not updated for `SAGA_TIMEOUT` and releases their stock. It uses `FOR UPDATE SKIP LOCKED`, so several instances can sweep at once. This covers a command that exhausted its redeliveries and never produced a reply (the gap noted in ADR-0012).

**Cancellation rule.** A customer may cancel while `PENDING` or `STOCK_RESERVED`; a `CONFIRMED` order cannot be cancelled, because that needs a refund.

**Prices come from the catalogue** (a gRPC call to inventory, made outside any transaction, in parallel, with a deadline per call). The client's prices are ignored.

## Known gap: refunds
If a payment succeeds for an order that is already cancelled (the customer cancelled, or the sweeper timed out, while the charge was in flight), the customer has paid for nothing. The order is marked `refund_required` and an error is logged, but money is not returned automatically. A refund flow (a `RefundPayment` command and its trigger) is future work. The window is small: it needs a cancellation or timeout to coincide with an in-flight charge.

## Alternatives considered
- **Forbid cancellation while `STOCK_RESERVED`:** closes most of the refund window, but the customer could not cancel for the moments between reservation and payment result, and the timeout path would still race.
- **Reserve a "payment pending" hold and refund on conflict:** needs the refund flow anyway.
- **Choreography:** no single place to reason about these races (ADR-0004).

## Consequences
- Any message can be replayed or reordered without corrupting an order.
- No order stays unfinished forever, and no stock stays locked for a cancelled order.
- One documented failure mode (payment succeeding for a cancelled order) is detected and flagged but not yet repaired.
