package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
)

var _ messaging.Observer = (*MessagingObserver)(nil)

// MessagingObserver turns what pkg/messaging reports into Prometheus metrics:
// how many messages each consumer handled and how they ended (ack, retry,
// terminated), how long handlers take, and how many publishes succeeded.
type MessagingObserver struct {
	consumed       *prometheus.CounterVec
	handling       *prometheus.HistogramVec
	published      *prometheus.CounterVec
	publishSeconds *prometheus.HistogramVec
}

// NewMessagingObserver registers the messaging metrics on reg.
func NewMessagingObserver(reg prometheus.Registerer) *MessagingObserver {
	f := promauto.With(reg)
	return &MessagingObserver{
		consumed: f.NewCounterVec(prometheus.CounterOpts{
			Name: "messaging_consumed_total",
			Help: "Messages consumed, by consumer, subject and outcome (ack, retry, terminated).",
		}, []string{"consumer", "subject", "outcome"}),
		handling: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "messaging_handling_seconds", Help: "Time spent handling a consumed message.", Buckets: latencyBuckets,
		}, []string{"consumer", "subject"}),
		published: f.NewCounterVec(prometheus.CounterOpts{
			Name: "messaging_published_total", Help: "Messages published to the broker, by subject and result (ok, error).",
		}, []string{"subject", "result"}),
		publishSeconds: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "messaging_publish_seconds", Help: "Time spent publishing a message and waiting for the broker.", Buckets: latencyBuckets,
		}, []string{"subject"}),
	}
}

// Consumed implements messaging.Observer.
func (o *MessagingObserver) Consumed(consumer, subject string, outcome messaging.Outcome, took time.Duration) {
	o.consumed.WithLabelValues(consumer, subject, string(outcome)).Inc()
	o.handling.WithLabelValues(consumer, subject).Observe(took.Seconds())
}

// Published implements messaging.Observer.
func (o *MessagingObserver) Published(subject string, ok bool, took time.Duration) {
	result := "ok"
	if !ok {
		result = "error"
	}
	o.published.WithLabelValues(subject, result).Inc()
	o.publishSeconds.WithLabelValues(subject).Observe(took.Seconds())
}
