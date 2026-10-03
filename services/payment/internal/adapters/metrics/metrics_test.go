package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
)

func TestRecorderCountsPaymentsAndProviderCalls(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)

	r.Charge(app.OutcomeSucceeded)
	r.Charge(app.OutcomeSucceeded)
	r.Charge(app.OutcomeFailed)
	r.Charge(app.OutcomeDuplicate)
	r.ProviderCall(app.ProviderSucceeded, 40*time.Millisecond)
	r.ProviderCall(app.ProviderSucceeded, 60*time.Millisecond)
	r.ProviderCall(app.ProviderDeclined, 30*time.Millisecond)
	r.ProviderCall(app.ProviderError, 5*time.Second)

	expected := `
# HELP payment_provider_calls_total Calls to the payment provider, by outcome: succeeded, declined or error (outage or timeout).
# TYPE payment_provider_calls_total counter
payment_provider_calls_total{outcome="declined"} 1
payment_provider_calls_total{outcome="error"} 1
payment_provider_calls_total{outcome="succeeded"} 2
# HELP payments_total Charge commands handled, by outcome: succeeded, failed (declined) or duplicate (already handled).
# TYPE payments_total counter
payments_total{outcome="duplicate"} 1
payments_total{outcome="failed"} 1
payments_total{outcome="succeeded"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "payments_total", "payment_provider_calls_total"); err != nil {
		t.Fatal(err)
	}
	// Latency is recorded per outcome, so a slow failing provider stands out from a fast healthy one.
	if n := testutil.CollectAndCount(r.providerLatency); n != 3 {
		t.Fatalf("latency series = %d, want one per outcome (3)", n)
	}
}
