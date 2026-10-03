# ADR-0016: Observability: OpenTelemetry traces, Prometheus metrics, tested alerts

**Status:** Accepted

## Context
An order crosses five services and four message hops. When one is slow or lost, logs from each service alone cannot say where, and unit tests cannot say whether the system is healthy right now.

## Decision
**Traces: OpenTelemetry, one trace per order.** The gateway starts a span for each request (named after the route pattern, never the path) and returns its trace ID in `X-Trace-Id`. gRPC calls propagate the trace with the otelgrpc handlers. Messages carry a W3C `traceparent` header. Because messages are published from the outbox, long after the request that created them, the trace context is **captured when the message is created and stored in the outbox row**; the relay's publish span continues from it and hands its own context to the consumer. The result is a single trace from the HTTP request through reservation, payment, confirmation and notification, including the rollback path when payment fails. Business IDs are span attributes (`order.id`), so a trace can be found by order ID. Spans go straight to Jaeger over OTLP; with no endpoint configured tracing is off but context is still propagated.

**Metrics: Prometheus client, a lean set.**
- *RED* for every gRPC server and client call and for every HTTP route, labelled by route **pattern** (`GET /v1/orders/{id}`), and unknown paths share one `unmatched` label so scanners cannot create unbounded series.
- *Messaging:* consumed (ack, retry, terminated), handling time, published (ok, error).
- *Business:* orders created and finished by outcome and **cause** (cancellation reasons are free text, so they are mapped to `payment_failed`, `out_of_stock`, `timeout`, `customer`), saga duration, refunds required, reservations, payments and provider calls, notifications per channel.
- *Outbox:* pending count and age of the oldest unpublished message.
- Business metrics are recorded **after the transaction commits**, so a step that is rolled back and retried is not counted twice.
- Go runtime and database pool metrics are deliberately left out for now.

**Where metrics are served.** Backend services serve `/metrics` on their internal health port. The gateway's main port is public, so it serves metrics on a separate internal port (`METRICS_ADDR`), and its configuration refuses to use the same address.

**Logs join the trace.** Log lines written with a request's context carry `trace_id` and `span_id`, so a trace leads to its log lines.

**Alerts are code and are tested.** Eight rules (service down, outbox stuck, saga timeouts, refund required, terminated messages, high cancellation rate, payment provider errors, gateway 5xx) live in `deploy/prometheus/rules` and are unit-tested with `promtool` against synthetic series, including the cases where they must stay quiet (low traffic, declines that are not provider errors). Each has a summary and a description saying where to look.

**Local stack.** Jaeger (all-in-one), Prometheus and Grafana run in Compose; Grafana is provisioned with both data sources and an overview dashboard.

## Alternatives considered
- **OpenTelemetry for metrics too:** one SDK, but a heavier dependency and a Prometheus exporter in between. The Prometheus client is simpler and what the dashboards and alerts speak natively.
- **Reading the trace context at publish time:** loses the link, because the relay runs without the request's context.
- **A collector between services and Jaeger:** useful in production (batching, sampling, fan-out), unnecessary here.
- **Labelling metrics with raw paths or free-text reasons:** easy, and an unbounded-cardinality incident waiting to happen.

## Consequences
- One order is one trace; compensation and timeouts are visible in it.
- NATS canonicalises header names (`traceparent` becomes `Traceparent`), so the propagator uses a case-insensitive carrier. This is covered by a test over a real broker.
- Sampling defaults to 100% (`OTEL_TRACES_SAMPLE_RATIO`), right for a demo; a busy deployment would sample.
- In-memory Jaeger and 2-day Prometheus retention are for development, not production.
- **Finding:** the trace shows roughly 300-400 ms between a service committing an event and the next service consuming it. That is the outbox relay's 500 ms poll interval, not slow code, and makes a whole saga take about 1.1 s. Shortening the interval, or waking the relay when a row is written, would cut that at the price of more idle polling.
