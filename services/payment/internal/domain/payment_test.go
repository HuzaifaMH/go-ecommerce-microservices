package domain

import (
	"errors"
	"testing"
)

func TestNewChargeRequestValidation(t *testing.T) {
	usd := func(n int64) Money { return Money{CurrencyCode: "USD", AmountMinor: n} }

	tests := []struct {
		name                string
		orderID, customerID string
		amount              Money
		wantErr             bool
	}{
		{"valid", "o-1", "c-1", usd(1999), false},
		{"no order", "", "c-1", usd(1999), true},
		{"no customer", "o-1", "", usd(1999), true},
		{"zero amount", "o-1", "c-1", usd(0), true},
		{"negative amount", "o-1", "c-1", usd(-5), true},
		{"lower-case currency", "o-1", "c-1", Money{"usd", 100}, true},
		{"two-letter currency", "o-1", "c-1", Money{"US", 100}, true},
		{"empty currency", "o-1", "c-1", Money{"", 100}, true},
		{"digits in currency", "o-1", "c-1", Money{"U5D", 100}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := NewChargeRequest(tc.orderID, tc.customerID, tc.amount)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidPayment) {
					t.Fatalf("err = %v, want ErrInvalidPayment", err)
				}
				return
			}
			if err != nil || req.OrderID != tc.orderID || req.Amount != tc.amount {
				t.Fatalf("req = %+v, err = %v", req, err)
			}
		})
	}
}

func TestNewSucceededAndFailed(t *testing.T) {
	req, _ := NewChargeRequest("o-1", "c-1", Money{"USD", 500})

	ok := NewSucceeded("p-1", req, "ref-9")
	if ok.Status != StatusSucceeded || ok.ProviderRef != "ref-9" || ok.FailureReason != "" || ok.OrderID != "o-1" {
		t.Errorf("succeeded = %+v", ok)
	}

	bad := NewFailed("p-2", req, "card declined")
	if bad.Status != StatusFailed || bad.FailureReason != "card declined" || bad.ProviderRef != "" {
		t.Errorf("failed = %+v", bad)
	}
}

func TestPaymentMatches(t *testing.T) {
	req, _ := NewChargeRequest("o-1", "c-1", Money{"USD", 500})
	p := NewSucceeded("p-1", req, "r")

	if !p.Matches(req) {
		t.Error("payment should match its own request")
	}
	for name, other := range map[string]ChargeRequest{
		"different amount":   {OrderID: "o-1", CustomerID: "c-1", Amount: Money{"USD", 501}},
		"different currency": {OrderID: "o-1", CustomerID: "c-1", Amount: Money{"EUR", 500}},
		"different customer": {OrderID: "o-1", CustomerID: "c-2", Amount: Money{"USD", 500}},
		"different order":    {OrderID: "o-2", CustomerID: "c-1", Amount: Money{"USD", 500}},
	} {
		if p.Matches(other) {
			t.Errorf("%s must not match", name)
		}
	}
}

func TestDeclinedErrorIsDistinguishable(t *testing.T) {
	var err error = &DeclinedError{Reason: "insufficient funds"}
	var declined *DeclinedError
	if !errors.As(err, &declined) || declined.Reason != "insufficient funds" {
		t.Fatalf("errors.As failed for %v", err)
	}
}
