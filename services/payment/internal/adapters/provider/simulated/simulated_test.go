package simulated

import (
	"context"
	"errors"
	"testing"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

func req(order, customer string, amount int64) domain.ChargeRequest {
	return domain.ChargeRequest{OrderID: order, CustomerID: customer, Amount: domain.Money{CurrencyCode: "USD", AmountMinor: amount}}
}

func TestSuccessIsIdempotentPerOrder(t *testing.T) {
	p := New(10_000)
	for range 3 {
		ref, err := p.Charge(context.Background(), req("o-1", "alice", 500))
		if err != nil || ref != "sim_o-1" {
			t.Fatalf("ref = %q, err = %v", ref, err)
		}
	}
}

func TestDeclines(t *testing.T) {
	p := New(10_000)
	tests := []struct {
		name string
		req  domain.ChargeRequest
		want string
	}{
		{"decline prefix", req("o-1", "decline-bob", 500), "card declined"},
		{"over the limit", req("o-2", "alice", 10_001), "amount exceeds limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Charge(context.Background(), tc.req)
			var declined *domain.DeclinedError
			if !errors.As(err, &declined) || declined.Reason != tc.want {
				t.Fatalf("err = %v, want decline %q", err, tc.want)
			}
		})
	}
}

func TestAmountAtTheLimitIsAccepted(t *testing.T) {
	if _, err := New(10_000).Charge(context.Background(), req("o-1", "alice", 10_000)); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestFlakyCustomerFailsOncePerOrderThenSucceeds(t *testing.T) {
	p := New(10_000)
	ctx := context.Background()

	_, err := p.Charge(ctx, req("o-1", "flaky-carol", 500))
	var declined *domain.DeclinedError
	if err == nil || errors.As(err, &declined) {
		t.Fatalf("first attempt must be a retryable outage, got %v", err)
	}
	if ref, err := p.Charge(ctx, req("o-1", "flaky-carol", 500)); err != nil || ref != "sim_o-1" {
		t.Fatalf("second attempt = %q, %v", ref, err)
	}
	// A different order is flaky again, independently.
	if _, err := p.Charge(ctx, req("o-2", "flaky-carol", 500)); err == nil {
		t.Fatal("first attempt for another order must fail too")
	}
}

func TestCancelledContextIsHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(10_000).Charge(ctx, req("o-1", "alice", 500)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
