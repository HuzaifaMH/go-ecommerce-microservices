package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

func TestRecorderCountsDeliveriesPerChannelAndOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)

	r.Delivery(domain.ChannelEmail, app.OutcomeSent)
	r.Delivery(domain.ChannelEmail, app.OutcomeSent)
	r.Delivery(domain.ChannelSMS, app.OutcomeSent)
	r.Delivery(domain.ChannelSMS, app.OutcomeRetry)
	r.Delivery(domain.ChannelSMS, app.OutcomeFailed)

	expected := `
# HELP notifications_total Delivery attempts, by channel (email, sms) and outcome: sent, failed (undeliverable) or retry (transient failure).
# TYPE notifications_total counter
notifications_total{channel="email",outcome="sent"} 2
notifications_total{channel="sms",outcome="failed"} 1
notifications_total{channel="sms",outcome="retry"} 1
notifications_total{channel="sms",outcome="sent"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
