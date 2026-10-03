// Package metrics exposes the notification service's business metrics to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

var _ app.Metrics = (*Recorder)(nil)

// Recorder implements app.Metrics on a Prometheus registry.
type Recorder struct {
	deliveries *prometheus.CounterVec
}

// New registers the notification metrics on reg.
func New(reg prometheus.Registerer) *Recorder {
	return &Recorder{
		deliveries: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_total",
			Help: "Delivery attempts, by channel (email, sms) and outcome: sent, failed (undeliverable) or retry (transient failure).",
		}, []string{"channel", "outcome"}),
	}
}

// Delivery implements app.Metrics.
func (r *Recorder) Delivery(channel domain.Channel, outcome string) {
	r.deliveries.WithLabelValues(string(channel), outcome).Inc()
}
