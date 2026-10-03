package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/ratelimit"
)

func serveProducts(h *harness) {
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{Sku: "A", UnitPrice: &commonv1.Money{CurrencyCode: "USD"}}}, nil
	}
}

func TestHTTPMetricsAreLabelledByRoutePatternNotByPath(t *testing.T) {
	m := NewHTTPMetrics(prometheus.NewRegistry())
	h := newHarness(t, options{metrics: m})
	serveProducts(h)

	// Many different concrete paths, one route: one series.
	for _, sku := range []string{"MUG", "BOOK", "SHIRT", "STICKER"} {
		if rec := h.do(req{method: "GET", path: "/v1/products/" + sku}); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "GET /v1/products/{sku}", "200")); got != 4 {
		t.Fatalf("count = %v, want 4 under the route pattern", got)
	}

	// Rejected before reaching a handler: still counted, under the route it targeted.
	h.do(req{method: "GET", path: "/v1/orders"}) // no token
	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "GET /v1/orders", "401")); got != 1 {
		t.Errorf("a 401 must be counted under its route, got %v", got)
	}

	// Scanners: unknown paths and wrong methods share one label, so they cannot create unbounded series.
	for _, path := range []string{"/wp-admin", "/.env", "/v1/does/not/exist", "/phpmyadmin/index.php"} {
		h.do(req{method: "GET", path: path})
	}
	h.do(req{method: "DELETE", path: "/v1/products"})
	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", unmatchedRoute, "404")); got != 4 {
		t.Errorf("unmatched 404s = %v, want 4", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("DELETE", unmatchedRoute, "405")); got != 1 {
		t.Errorf("unmatched 405s = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.requests); n != 4 { // products 200, orders 401, unmatched GET 404, unmatched DELETE 405
		t.Errorf("%d request series, want 4: attacker-chosen paths must not become labels", n)
	}
	if n := testutil.CollectAndCount(m.duration); n < 1 {
		t.Error("durations must be recorded")
	}
	if got := testutil.ToFloat64(m.inFlight); got != 0 {
		t.Errorf("in flight = %v after all requests finished", got)
	}
}

func TestRateLimitedRequestsAreCountedToo(t *testing.T) {
	m := NewHTTPMetrics(prometheus.NewRegistry())
	h := newHarness(t, options{metrics: m, limiter: ratelimit.New(1, 1, ratelimit.Options{Now: func() time.Time { return created }})})
	serveProducts(h)

	h.do(req{method: "GET", path: "/v1/products/A"})
	h.do(req{method: "GET", path: "/v1/products/A"}) // over the limit
	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "GET /v1/products/{sku}", "429")); got != 1 {
		t.Fatalf("429s = %v, want 1: throttling must be visible in the metrics", got)
	}
}

func TestMetricsAreOptional(t *testing.T) {
	h := newHarness(t) // no metrics configured
	serveProducts(h)
	if rec := h.do(req{method: "GET", path: "/v1/products/A"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// --- tracing ---

func tracedHarness(t *testing.T) (*harness, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp) // before the handler is built: it picks the provider up then
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	h := newHarness(t)
	serveProducts(h)
	return h, rec
}

func TestARequestContinuesTheCallersTraceAndReturnsItsID(t *testing.T) {
	h, rec := tracedHarness(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	resp := h.do(req{method: "GET", path: "/v1/products/MUG", headers: map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
	}})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if got := resp.Header().Get("X-Trace-Id"); got != traceID {
		t.Fatalf("X-Trace-Id = %q, want the caller's trace %s", got, traceID)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans", len(spans))
	}
	if got := spans[0].Name(); got != "GET /v1/products/{sku}" {
		t.Errorf("span name = %q, want exactly the route pattern (not the path, which contains IDs, and without the method twice)", got)
	}
	if spans[0].SpanContext().TraceID().String() != traceID {
		t.Errorf("span is in trace %s, want the caller's", spans[0].SpanContext().TraceID())
	}
}

func TestARequestWithoutATraceStartsOne(t *testing.T) {
	h, _ := tracedHarness(t)
	resp := h.do(req{method: "GET", path: "/v1/products/MUG"})
	if got := resp.Header().Get("X-Trace-Id"); len(got) != 32 {
		t.Fatalf("X-Trace-Id = %q, want a new 32-hex-digit trace ID", got)
	}
}

func TestCreatingAnOrderTagsTheTraceWithTheOrderID(t *testing.T) {
	h, rec := tracedHarness(t)
	h.orders.create = func(in *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
		return &orderv1.CreateOrderResponse{Order: protoOrder("order-123", in.GetCustomerId(), "pending")}, nil
	}

	resp := h.do(req{
		method: "POST", path: "/v1/orders", body: `{"items":[{"sku":"A","quantity":1}]}`,
		headers: map[string]string{"Authorization": "Bearer " + h.token("alice"), "Idempotency-Key": "k"},
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body)
	}

	found := false
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "order.id" && kv.Value.AsString() == "order-123" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the request's span must carry order.id, so the trace can be found by order ID")
	}
}
