// Package metrics exposes the payment service's business metrics to Prometheus.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
)

var _ app.Metrics = (*Recorder)(nil)

// Recorder implements app.Metrics on a Prometheus registry.
type Recorder struct {
	payments        *prometheus.CounterVec
	providerCalls   *prometheus.CounterVec
	providerLatency *prometheus.HistogramVec
}

// New registers the payment metrics on reg.
func New(reg prometheus.Registerer) *Recorder {
	f := promauto.With(reg)
	return &Recorder{
		payments: f.NewCounterVec(prometheus.CounterOpts{
			Name: "payments_total",
			Help: "Charge commands handled, by outcome: succeeded, failed (declined) or duplicate (already handled).",
		}, []string{"outcome"}),
		providerCalls: f.NewCounterVec(prometheus.CounterOpts{
			Name: "payment_provider_calls_total",
			Help: "Calls to the payment provider, by outcome: succeeded, declined or error (outage or timeout).",
		}, []string{"outcome"}),
		providerLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "payment_provider_seconds", Help: "Time spent in calls to the payment provider.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"outcome"}),
	}
}

// Charge implements app.Metrics.
func (r *Recorder) Charge(outcome string) { r.payments.WithLabelValues(outcome).Inc() }

// ProviderCall implements app.Metrics.
func (r *Recorder) ProviderCall(outcome string, took time.Duration) {
	r.providerCalls.WithLabelValues(outcome).Inc()
	r.providerLatency.WithLabelValues(outcome).Observe(took.Seconds())
}
