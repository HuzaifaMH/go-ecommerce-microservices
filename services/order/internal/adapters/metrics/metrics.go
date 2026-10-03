// Package metrics exposes the order service's business metrics to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

var _ app.Metrics = (*Recorder)(nil)

// sagaBuckets cover a saga that takes a few hundred milliseconds up to the
// default two-minute timeout.
var sagaBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Recorder implements app.Metrics on a Prometheus registry.
type Recorder struct {
	created  prometheus.Counter
	finished *prometheus.CounterVec
	duration *prometheus.HistogramVec
	refunds  prometheus.Counter
}

// New registers the order metrics on reg.
func New(reg prometheus.Registerer) *Recorder {
	f := promauto.With(reg)
	return &Recorder{
		created: f.NewCounter(prometheus.CounterOpts{
			Name: "orders_created_total", Help: "Orders accepted (idempotent repeats are not counted).",
		}),
		finished: f.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_finished_total",
			Help: "Orders that reached a final state. outcome is confirmed or cancelled; reason is payment_failed, out_of_stock, timeout or customer for cancellations, none for confirmations.",
		}, []string{"outcome", "reason"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "order_saga_duration_seconds", Help: "Time from order creation to its final state.", Buckets: sagaBuckets,
		}, []string{"outcome"}),
		refunds: f.NewCounter(prometheus.CounterOpts{
			Name: "orders_refund_required_total",
			Help: "Payments that succeeded for an order that was already cancelled; the customer must be refunded.",
		}),
	}
}

// OrderCreated implements app.Metrics.
func (r *Recorder) OrderCreated() { r.created.Inc() }

// OrderFinished implements app.Metrics.
func (r *Recorder) OrderFinished(o domain.Order) {
	outcome, reason := "confirmed", "none"
	if o.Status == domain.StatusCancelled {
		outcome, reason = "cancelled", domain.CancelCause(o.CancelReason)
	}
	r.finished.WithLabelValues(outcome, reason).Inc()
	r.duration.WithLabelValues(outcome).Observe(o.UpdatedAt.Sub(o.CreatedAt).Seconds())
}

// RefundRequired implements app.Metrics.
func (r *Recorder) RefundRequired() { r.refunds.Inc() }
