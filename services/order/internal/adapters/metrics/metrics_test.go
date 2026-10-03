package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

var created = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func order(status domain.Status, reason string, took time.Duration) domain.Order {
	return domain.Order{Status: status, CancelReason: reason, CreatedAt: created, UpdatedAt: created.Add(took)}
}

func TestRecorderCountsOutcomesWithBoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)

	r.OrderCreated()
	r.OrderCreated()
	r.OrderFinished(order(domain.StatusConfirmed, "", 1500*time.Millisecond))
	r.OrderFinished(order(domain.StatusCancelled, "payment failed: card declined", time.Second))
	r.OrderFinished(order(domain.StatusCancelled, "payment failed: insufficient funds", time.Second)) // same cause, different text
	r.OrderFinished(order(domain.StatusCancelled, "saga timeout", 2*time.Minute))
	r.OrderFinished(order(domain.StatusCancelled, "typed by a customer "+strings.Repeat("z", 200), time.Second))
	r.RefundRequired()

	expected := `
# HELP orders_created_total Orders accepted (idempotent repeats are not counted).
# TYPE orders_created_total counter
orders_created_total 2
# HELP orders_finished_total Orders that reached a final state. outcome is confirmed or cancelled; reason is payment_failed, out_of_stock, timeout or customer for cancellations, none for confirmations.
# TYPE orders_finished_total counter
orders_finished_total{outcome="cancelled",reason="customer"} 1
orders_finished_total{outcome="cancelled",reason="payment_failed"} 2
orders_finished_total{outcome="cancelled",reason="timeout"} 1
orders_finished_total{outcome="confirmed",reason="none"} 1
# HELP orders_refund_required_total Payments that succeeded for an order that was already cancelled; the customer must be refunded.
# TYPE orders_refund_required_total counter
orders_refund_required_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "orders_created_total", "orders_finished_total", "orders_refund_required_total"); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderObservesSagaDurationPerOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)
	r.OrderFinished(order(domain.StatusConfirmed, "", 1500*time.Millisecond))
	r.OrderFinished(order(domain.StatusCancelled, "saga timeout", 2*time.Minute))

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "order_saga_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			sums[m.GetLabel()[0].GetValue()] = m.GetHistogram().GetSampleSum()
		}
	}
	if sums["confirmed"] != 1.5 || sums["cancelled"] != 120 {
		t.Fatalf("duration sums = %v, want confirmed 1.5s and cancelled 120s", sums)
	}
}
