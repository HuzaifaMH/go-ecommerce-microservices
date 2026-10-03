package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// BacklogSource reports how many outbox messages are waiting to be published
// and how long the oldest has waited. pgstore.Store implements it.
type BacklogSource interface {
	Backlog(ctx context.Context) (pending int64, oldest time.Duration, err error)
}

// OutboxGauges exposes the state of the transactional outbox. A backlog that
// grows, or a message that waits for a long time, means events are not
// reaching the broker, which stalls the saga.
type OutboxGauges struct {
	pending prometheus.Gauge
	oldest  prometheus.Gauge
}

// NewOutboxGauges registers the outbox gauges on reg.
func NewOutboxGauges(reg prometheus.Registerer) *OutboxGauges {
	f := promauto.With(reg)
	return &OutboxGauges{
		pending: f.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending_messages", Help: "Outbox messages not yet published to the broker.",
		}),
		oldest: f.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_oldest_pending_age_seconds", Help: "Age of the oldest unpublished outbox message; 0 when there is none.",
		}),
	}
}

// Run refreshes the gauges every interval until ctx is cancelled. It is meant
// to be run as a runner.Task. A failed refresh is logged and leaves the
// previous values in place until the next tick.
func (g *OutboxGauges) Run(ctx context.Context, src BacklogSource, interval time.Duration, log *slog.Logger) error {
	g.refresh(ctx, src, log)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			g.refresh(ctx, src, log)
		}
	}
}

func (g *OutboxGauges) refresh(ctx context.Context, src BacklogSource, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pending, oldest, err := src.Backlog(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("could not read the outbox backlog", "error", err)
		}
		return
	}
	g.pending.Set(float64(pending))
	g.oldest.Set(oldest.Seconds())
}
