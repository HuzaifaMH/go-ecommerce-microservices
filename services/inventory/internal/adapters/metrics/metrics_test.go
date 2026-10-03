package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
)

func TestRecorderCountsByOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)

	for range 3 {
		r.Reservation(app.OutcomeReserved)
	}
	r.Reservation(app.OutcomeRejected)
	r.Reservation(app.OutcomeDuplicate)
	r.Release(app.OutcomeReleased)
	r.Release(app.OutcomeNoop)
	r.Release(app.OutcomeNoop)

	expected := `
# HELP stock_releases_total Release commands handled, by outcome: released or noop (nothing to release).
# TYPE stock_releases_total counter
stock_releases_total{outcome="noop"} 2
stock_releases_total{outcome="released"} 1
# HELP stock_reservations_total Reserve commands handled, by outcome: reserved, rejected (not enough stock) or duplicate (already handled).
# TYPE stock_reservations_total counter
stock_reservations_total{outcome="duplicate"} 1
stock_reservations_total{outcome="rejected"} 1
stock_reservations_total{outcome="reserved"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
