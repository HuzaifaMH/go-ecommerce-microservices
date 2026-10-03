// Package metrics exposes the inventory service's business metrics to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
)

var _ app.Metrics = (*Recorder)(nil)

// Recorder implements app.Metrics on a Prometheus registry.
type Recorder struct {
	reservations *prometheus.CounterVec
	releases     *prometheus.CounterVec
}

// New registers the inventory metrics on reg.
func New(reg prometheus.Registerer) *Recorder {
	f := promauto.With(reg)
	return &Recorder{
		reservations: f.NewCounterVec(prometheus.CounterOpts{
			Name: "stock_reservations_total",
			Help: "Reserve commands handled, by outcome: reserved, rejected (not enough stock) or duplicate (already handled).",
		}, []string{"outcome"}),
		releases: f.NewCounterVec(prometheus.CounterOpts{
			Name: "stock_releases_total",
			Help: "Release commands handled, by outcome: released or noop (nothing to release).",
		}, []string{"outcome"}),
	}
}

// Reservation implements app.Metrics.
func (r *Recorder) Reservation(outcome string) { r.reservations.WithLabelValues(outcome).Inc() }

// Release implements app.Metrics.
func (r *Recorder) Release(outcome string) { r.releases.WithLabelValues(outcome).Inc() }
