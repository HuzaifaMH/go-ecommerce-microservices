# ADR-0009: Observability with OpenTelemetry, Prometheus and Jaeger

**Status:** Accepted

## Context
Debugging an order that crosses five services requires correlated telemetry.

## Decision
Use OpenTelemetry for traces and metrics, propagating trace context through gRPC metadata and NATS headers. Prometheus and Grafana for metrics, Jaeger for traces, JSON `slog` logs carrying `trace_id`.

## Alternatives considered
Vendor agents or logs-only debugging: not portable or not correlated.

## Consequences
One order appears as a single trace across all services. Adds some instrumentation code and local infrastructure.
