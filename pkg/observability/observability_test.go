package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
)

// recordingTracer installs a tracer provider that keeps finished spans in memory.
func recordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestSetupTracingWithoutEndpointStillPropagatesContext(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), Config{Service: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A trace context arriving over the wire must survive passing through a
	// service that exports nothing.
	rec := recordingTracer(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "origin")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	span.End()

	if carrier.Get("traceparent") == "" {
		t.Fatalf("no traceparent injected: %v", carrier)
	}
	remote := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	_, child := otel.Tracer("test").Start(remote, "child")
	child.End()

	spans := rec.Ended()
	if len(spans) != 2 || spans[1].Parent().SpanID() != spans[0].SpanContext().SpanID() || spans[1].SpanContext().TraceID() != spans[0].SpanContext().TraceID() {
		t.Fatalf("the child must continue the same trace: %+v", spans)
	}
}

func TestSetupTracingRejectsAnInvalidSampleRatio(t *testing.T) {
	for _, ratio := range []float64{-0.1, 1.5} {
		if _, err := SetupTracing(context.Background(), Config{Service: "svc", OTLPEndpoint: "http://localhost:4317", SampleRatio: ratio}); err == nil {
			t.Errorf("ratio %v must be rejected", ratio)
		}
	}
}

func TestSetupTracingWithAnEndpointInstallsAProviderAndShutsDownCleanly(t *testing.T) {
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	// The exporter connects lazily, so no collector needs to be running.
	shutdown, err := SetupTracing(context.Background(), Config{Service: "svc", Version: "1.2.3", OTLPEndpoint: "http://localhost:4317", SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("provider = %T, want the SDK provider", otel.GetTracerProvider())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg := ConfigFromEnv(config.FromMap(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://jaeger:4317", "OTEL_TRACES_SAMPLE_RATIO": "0.25"}), "order-service")
	if cfg.Service != "order-service" || cfg.OTLPEndpoint != "http://jaeger:4317" || cfg.SampleRatio != 0.25 {
		t.Fatalf("cfg = %+v", cfg)
	}
	def := ConfigFromEnv(config.FromMap(nil), "x")
	if def.OTLPEndpoint != "" || def.SampleRatio != 1 {
		t.Fatalf("defaults = %+v: tracing must be off unless an endpoint is set, and sample everything", def)
	}
}

func TestSetSpanAttrs(t *testing.T) {
	rec := recordingTracer(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "op")
	SetSpanAttrs(ctx, "order.id", "o-1", "customer.id", "alice", "dangling")
	span.End()

	attrs := map[string]string{}
	for _, kv := range rec.Ended()[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["order.id"] != "o-1" || attrs["customer.id"] != "alice" || len(attrs) != 2 {
		t.Fatalf("attributes = %v (an unpaired trailing key must be ignored)", attrs)
	}

	// No span in the context: must not panic.
	SetSpanAttrs(context.Background(), "order.id", "o-1")
}

func TestRecordErrorMarksTheSpanFailed(t *testing.T) {
	rec := recordingTracer(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "op")
	RecordError(ctx, nil) // nothing to record
	RecordError(ctx, errors.New("boom"))
	span.End()

	s := rec.Ended()[0]
	if s.Status().Code.String() != "Error" || s.Status().Description != "boom" || len(s.Events()) != 1 {
		t.Fatalf("status = %+v, events = %d", s.Status(), len(s.Events()))
	}
	RecordError(context.Background(), errors.New("no span")) // must not panic
}

func TestTraceID(t *testing.T) {
	if TraceID(context.Background()) != "" {
		t.Error("no trace: empty ID expected")
	}
	rec := recordingTracer(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()
	if got := TraceID(ctx); got == "" || got != rec.Ended()[0].SpanContext().TraceID().String() {
		t.Fatalf("TraceID = %q", got)
	}
	_ = noop.NewTracerProvider() // keep the noop import honest: a no-op span has no valid trace ID
	if TraceID(trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})) != "" {
		t.Error("an invalid span context has no trace ID")
	}
}

// --- gRPC metrics ---

func TestGRPCServerInterceptorCountsByCodeAndTimesCalls(t *testing.T) {
	reg := NewRegistry()
	m := NewGRPCMetrics(reg)
	intercept := m.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/ecommerce.order.v1.OrderService/CreateOrder"}

	ok := func(context.Context, any) (any, error) { return "resp", nil }
	notFound := func(context.Context, any) (any, error) { return nil, status.Error(codes.NotFound, "x") }
	plain := func(context.Context, any) (any, error) { return nil, errors.New("not a status error") }

	for range 3 {
		if _, err := intercept(context.Background(), nil, info, ok); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = intercept(context.Background(), nil, info, notFound)
	_, _ = intercept(context.Background(), nil, info, plain)

	count := func(code string) float64 {
		return testutil.ToFloat64(m.serverHandled.WithLabelValues(info.FullMethod, code))
	}
	if count("OK") != 3 || count("NotFound") != 1 || count("Unknown") != 1 {
		t.Fatalf("OK=%v NotFound=%v Unknown=%v", count("OK"), count("NotFound"), count("Unknown"))
	}
	if n := testutil.CollectAndCount(m.serverDuration); n != 1 {
		t.Fatalf("duration histogram has %d series, want 1 per method", n)
	}
}

func TestGRPCClientInterceptorCountsCallsToOtherServices(t *testing.T) {
	m := NewGRPCMetrics(NewRegistry())
	intercept := m.UnaryClientInterceptor()

	fail := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return status.Error(codes.Unavailable, "down")
	}
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return fail(ctx, method, req, reply, cc, opts...)
	}
	if err := intercept(context.Background(), "/inventory.v1.InventoryService/GetStock", nil, nil, nil, invoker); status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v: the interceptor must not change the result", err)
	}
	if got := testutil.ToFloat64(m.clientHandled.WithLabelValues("/inventory.v1.InventoryService/GetStock", "Unavailable")); got != 1 {
		t.Fatalf("client Unavailable count = %v", got)
	}
}

func TestMetricsHandlerServesMetricsAndLeavesOtherPathsToTheNextHandler(t *testing.T) {
	reg := NewRegistry()
	m := NewGRPCMetrics(reg)
	m.serverHandled.WithLabelValues("/svc/Method", "OK").Inc()

	h := WithMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "health") }), reg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `grpc_server_handled_total{grpc_code="OK",grpc_method="/svc/Method"} 1`) {
		t.Fatalf("/metrics: %d\n%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Error("only the metrics this system defines should be exposed (lean set)")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))
	if rec.Body.String() != "health" {
		t.Fatalf("/readyz was not passed on: %q", rec.Body)
	}
}

// --- instruments: a trace must cross a real gRPC call ---

type pingServer struct {
	healthpb.UnimplementedHealthServer
	traceIDs chan string
}

func (p *pingServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	p.traceIDs <- TraceID(ctx)
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func TestATraceCrossesAGRPCCallAndTheCallIsMeasured(t *testing.T) {
	rec := recordingTracer(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // tracing stays under this test's control

	in, err := Start(context.Background(), "test-service")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(time.Second) })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(in.ServerOptions()...)
	ping := &pingServer{traceIDs: make(chan string, 1)}
	healthpb.RegisterHealthServer(srv, ping)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet", append(in.DialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, root := otel.Tracer("test").Start(context.Background(), "incoming request")
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	root.End()

	if got, want := <-ping.traceIDs, root.SpanContext().TraceID().String(); got != want {
		t.Fatalf("the server handled the call in trace %s, want the caller's trace %s", got, want)
	}

	// Client and server each recorded a span, all in the one trace.
	deadline := time.Now().Add(3 * time.Second)
	for len(rec.Ended()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("span %q is in a different trace", s.Name())
		}
	}
	if len(rec.Ended()) < 3 {
		t.Fatalf("recorded %d spans, want the request, the client call and the server call", len(rec.Ended()))
	}

	// And both sides were measured.
	method := "/grpc.health.v1.Health/Check"
	if got := testutil.ToFloat64(in.grpc.serverHandled.WithLabelValues(method, "OK")); got != 1 {
		t.Errorf("server call count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(in.grpc.clientHandled.WithLabelValues(method, "OK")); got != 1 {
		t.Errorf("client call count = %v, want 1", got)
	}
}

func TestStartRejectsABadSampleRatioFromTheEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")
	t.Setenv("OTEL_TRACES_SAMPLE_RATIO", "2")
	if _, err := Start(context.Background(), "svc"); err == nil {
		t.Fatal("a sample ratio above 1 must be rejected at start-up")
	}
	t.Setenv("OTEL_TRACES_SAMPLE_RATIO", "lots")
	if _, err := Start(context.Background(), "svc"); err == nil {
		t.Fatal("an unparsable sample ratio must be rejected at start-up")
	}
}

// --- outbox gauges ---

// fakeBacklog is read by the gauge loop's goroutine while the test changes it,
// so access is guarded.
type fakeBacklog struct {
	mu      sync.Mutex
	pending int64
	oldest  time.Duration
	err     error
}

func (f *fakeBacklog) Backlog(context.Context) (int64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, f.oldest, f.err
}

func (f *fakeBacklog) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func TestOutboxGaugesFollowTheBacklogAndSurviveErrors(t *testing.T) {
	reg := NewRegistry()
	g := NewOutboxGauges(reg)
	src := &fakeBacklog{pending: 7, oldest: 90 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx, src, 10*time.Millisecond, discard) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the first values", func() bool { return gaugeValue(t, reg, "outbox_pending_messages") == 7 })
	if got := gaugeValue(t, reg, "outbox_oldest_pending_age_seconds"); got != 90 {
		t.Errorf("oldest age = %v, want 90", got)
	}

	// A failing database must not zero the gauges: that would hide the very problem being reported.
	src.setErr(errors.New("database down"))
	time.Sleep(50 * time.Millisecond)
	if got := gaugeValue(t, reg, "outbox_pending_messages"); got != 7 {
		t.Errorf("pending = %v after a failed refresh, want the last known value 7", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}
