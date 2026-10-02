package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func ids() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("n-%d", n) }
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		in   Money
		want string
	}{
		{Money{"USD", 1999}, "19.99 USD"},
		{Money{"USD", 5}, "0.05 USD"},
		{Money{"EUR", 100}, "1.00 EUR"},
		{Money{"USD", 0}, "0.00 USD"},
		{Money{"USD", 123456789}, "1234567.89 USD"},
		{Money{"USD", -250}, "-2.50 USD"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOrderConfirmedIsOneEmail(t *testing.T) {
	ns, err := OrderConfirmed(ids(), "o-1", "alice", Money{"USD", 3000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("got %d notifications, want 1", len(ns))
	}
	n := ns[0]
	if n.Kind != KindOrderConfirmed || n.Channel != ChannelEmail || n.Recipient != "alice" || n.CustomerID != "alice" ||
		n.OrderID != "o-1" || n.Status != StatusPending || n.ID != "n-1" || !n.CreatedAt.Equal(now) || !n.UpdatedAt.Equal(now) {
		t.Fatalf("notification = %+v", n)
	}
	if !strings.Contains(n.Body, "o-1") || !strings.Contains(n.Body, "30.00 USD") {
		t.Errorf("body should name the order and the total: %q", n.Body)
	}
}

func TestOrderCancelledIsEmailAndSMSWithAnExplanation(t *testing.T) {
	ns, err := OrderCancelled(ids(), "o-1", "alice", "payment failed: card declined", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0].Channel != ChannelEmail || ns[1].Channel != ChannelSMS {
		t.Fatalf("notifications = %+v", ns)
	}
	if ns[0].ID == ns[1].ID {
		t.Error("each notification needs its own ID")
	}
	for _, n := range ns {
		if n.Kind != KindOrderCancelled || n.Recipient != "alice" || n.Status != StatusPending {
			t.Errorf("notification = %+v", n)
		}
		if !strings.Contains(n.Body, "o-1") || !strings.Contains(n.Body, "payment") {
			t.Errorf("body should name the order and explain the payment problem: %q", n.Body)
		}
	}
}

func TestExplain(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"payment failed: card declined", "could not process your payment"},
		{"out of stock: insufficient stock", "no longer available"},
		{"saga timeout", "too long"},
		{"", "No reason was given"},
		{"changed my mind", "Reason: changed my mind."},
		{"cancelled by customer", "Reason: cancelled by customer."},
	}
	for _, tc := range tests {
		if got := Explain(tc.reason); !strings.Contains(got, tc.want) {
			t.Errorf("Explain(%q) = %q, want it to contain %q", tc.reason, got, tc.want)
		}
	}
}

func TestInvalidEvents(t *testing.T) {
	if _, err := OrderConfirmed(ids(), "", "alice", Money{"USD", 1}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("confirmed without order: %v", err)
	}
	if _, err := OrderConfirmed(ids(), "o-1", "", Money{"USD", 1}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("confirmed without customer: %v", err)
	}
	if _, err := OrderCancelled(ids(), "", "alice", "x", now); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("cancelled without order: %v", err)
	}
	if _, err := OrderCancelled(ids(), "o-1", "", "x", now); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("cancelled without customer: %v", err)
	}
}

func TestUndeliverableErrorIsDistinguishable(t *testing.T) {
	var err error = &UndeliverableError{Reason: "mailbox does not exist"}
	var u *UndeliverableError
	if !errors.As(err, &u) || u.Reason != "mailbox does not exist" || !strings.Contains(err.Error(), "undeliverable") {
		t.Fatalf("err = %v", err)
	}
}
