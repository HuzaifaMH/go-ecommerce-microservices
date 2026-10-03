# Observability

`make up` starts the monitoring stack with the services.

| What | Where | Notes |
|---|---|---|
| **Jaeger** (traces) | http://localhost:16686 | One order is one trace across all services. Search service `api-gateway`, or the tag `order.id=<order id>` |
| **Grafana** (dashboards) | http://localhost:3000 | Opens on the "E-Commerce overview" dashboard (no login needed to view) |
| **Prometheus** (metrics, alerts) | http://localhost:9099 | Alert rules at `/alerts` |

## Traces

![One order as one trace](images/jaeger-trace.png)

Trace context travels through the outbox (stored in the row's headers) and through JetStream message headers, so the asynchronous saga stays in the same trace as the HTTP request that started it. Every API response carries `X-Trace-Id` and `X-Request-Id`; paste the trace ID into Jaeger. Log lines written during a request carry the same `trace_id`.

## Metrics and dashboard

![Grafana dashboard](images/grafana-dashboard.png)

Request rate, errors and duration for the gateway and gRPC; messaging throughput and outcomes per consumer; business metrics (orders confirmed and cancelled, by reason; saga duration); outbox backlog and the age of the oldest unpublished event. Metrics are recorded after the transaction commits, so a rolled-back transaction never counts.

## Alerts

Eight alert rules: `ServiceDown`, `OutboxStuck`, `OrderSagaTimeouts`, `RefundRequired`, `MessagesTerminated`, `OrderCancellationRateHigh`, `PaymentProviderErrors` and `GatewayHighErrorRate`. They are unit-tested with `promtool` (`make rules-test`), so a typo in a rule fails CI.

## Try the failure path

```bash
TOKEN=$(curl -s -X POST localhost:8080/dev/token -H 'Content-Type: application/json' -d '{"subject":"decline-dave"}' | jq -r .access_token)
curl -si -X POST localhost:8080/v1/orders -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: demo-2' \
     -H 'Content-Type: application/json' -d '{"items":[{"sku":"BOOK-GO-001","quantity":1}]}' | grep -i x-trace-id
```

A customer starting with `decline-` has payment refused. The trace shows the rollback: payment failed, stock released, order cancelled, customer notified.

Design and trade-offs: [ADR-0016](adr/0016-observability.md).
