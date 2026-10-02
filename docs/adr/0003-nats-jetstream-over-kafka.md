# ADR-0003: NATS JetStream over Kafka and RabbitMQ

**Status:** Accepted

## Context
The saga and notifications need durable, at-least-once messaging with consumer acknowledgements and de-duplication.

## Decision
Use NATS JetStream.

## Alternatives considered
Kafka: better for very high throughput, long retention with offset replay, and its connector and stream-processing ecosystem; none are requirements here and it is much heavier to run. RabbitMQ: strong routing, weaker replay semantics.

## Consequences
JetStream is a single small binary, has subject-based routing, durable pull consumers, explicit acks with redelivery, and publish de-duplication via `Nats-Msg-Id`, which the outbox relies on. The ecosystem is smaller. Revisit if we need large-scale replay or stream processing.
