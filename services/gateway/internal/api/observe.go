package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// unmatchedRoute labels requests that no route matched (unknown paths, wrong
// methods). Using one fixed label keeps scanners from creating unbounded
// metric series.
const unmatchedRoute = "unmatched"

// HTTPMetrics records rate, errors and duration of the REST API: the RED
// metrics, labelled by route pattern ("GET /v1/orders/{id}"), never by the
// concrete path, so the number of series stays bounded.
type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewHTTPMetrics registers the HTTP metrics on reg.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	f := promauto.With(reg)
	return &HTTPMetrics{
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests served, by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "Time taken to serve HTTP requests, by method and route pattern.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		inFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight", Help: "HTTP requests currently being served.",
		}),
	}
}

// middleware records every request, including those rejected before reaching a
// handler (authentication, rate limiting), which is where abuse shows up.
func (m *HTTPMetrics) middleware(mux *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.inFlight.Inc()
			defer m.inFlight.Dec()

			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			route := routePattern(mux, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			m.requests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
			m.duration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		})
	}
}

// routePattern returns the pattern of the route that serves r, or "unmatched".
func routePattern(mux *http.ServeMux, r *http.Request) string {
	if _, pattern := mux.Handler(r); pattern != "" {
		return pattern
	}
	return unmatchedRoute
}

// traced starts a server span for every request, named after the route
// pattern, continues a trace sent by the caller (traceparent header), and
// returns the trace ID in the X-Trace-Id response header so a customer
// complaint can be matched to a trace.
func traced(mux *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		withHeader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
				w.Header().Set("X-Trace-Id", sc.TraceID().String())
			}
			next.ServeHTTP(w, r)
		})
		return otelhttp.NewHandler(withHeader, "gateway",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				// A matched route pattern already starts with the method ("GET /v1/orders/{id}").
				if route := routePattern(mux, r); route != unmatchedRoute {
					return route
				}
				return r.Method + " " + unmatchedRoute
			}))
	}
}
