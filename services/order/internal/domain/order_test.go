package domain

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func usd(n int64) Money { return Money{CurrencyCode: "USD", AmountMinor: n} }

func prices() map[string]Money {
	return map[string]Money{"A": usd(1000), "B": usd(250), "EUR": {"EUR", 100}}
}

func newOrder(t *testing.T) Order {
	t.Helper()
	o, err := NewOrder("o-1", "c-1", "key-1", []Line{{"A", 2}, {"B", 4}}, prices(), now)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestNormalizeLinesMergesAndSorts(t *testing.T) {
	got, err := NormalizeLines([]Line{{"B", 1}, {"A", 2}, {"B", 3}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Line{{"A", 2}, {"B", 4}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormalizeLinesValidation(t *testing.T) {
	tests := map[string][]Line{
		"none":                  nil,
		"empty sku":             {{"", 1}},
		"zero quantity":         {{"A", 0}},
		"negative quantity":     {{"A", -1}},
		"quantity over limit":   {{"A", MaxQuantity + 1}},
		"merged over the limit": {{"A", MaxQuantity}, {"A", 1}},
	}
	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeLines(lines); !errors.Is(err, ErrInvalidOrder) {
				t.Fatalf("err = %v, want ErrInvalidOrder", err)
			}
		})
	}
}

func TestNewOrderPricesAndTotals(t *testing.T) {
	o := newOrder(t)

	if o.Status != StatusPending || o.Total != usd(3000) { // 2*1000 + 4*250
		t.Fatalf("order = %+v", o)
	}
	if want := []Item{{"A", 2, usd(1000)}, {"B", 4, usd(250)}}; !reflect.DeepEqual(o.Items, want) {
		t.Errorf("items = %v", o.Items)
	}
	if !o.CreatedAt.Equal(now) || !o.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v / %v", o.CreatedAt, o.UpdatedAt)
	}
}

func TestNewOrderErrors(t *testing.T) {
	tests := []struct {
		name              string
		id, customer, key string
		lines             []Line
		prices            map[string]Money
		wantInvalid       bool
		wantUnknownSKU    string
	}{
		{"no id", "", "c", "k", []Line{{"A", 1}}, prices(), true, ""},
		{"no customer", "o", "", "k", []Line{{"A", 1}}, prices(), true, ""},
		{"no idempotency key", "o", "c", "", []Line{{"A", 1}}, prices(), true, ""},
		{"unknown sku", "o", "c", "k", []Line{{"ZZZ", 1}}, prices(), false, "ZZZ"},
		{"mixed currencies", "o", "c", "k", []Line{{"A", 1}, {"EUR", 1}}, prices(), true, ""},
		{"negative price", "o", "c", "k", []Line{{"A", 1}}, map[string]Money{"A": usd(-1)}, true, ""},
		{"free order", "o", "c", "k", []Line{{"A", 1}}, map[string]Money{"A": usd(0)}, true, ""},
		{"line total overflows", "o", "c", "k", []Line{{"A", 2}}, map[string]Money{"A": usd(math.MaxInt64)}, true, ""},
		{"sum overflows", "o", "c", "k", []Line{{"A", 1}, {"B", 1}}, map[string]Money{"A": usd(math.MaxInt64), "B": usd(1)}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrder(tc.id, tc.customer, tc.key, tc.lines, tc.prices, now)
			var unknown *UnknownSKUError
			switch {
			case tc.wantInvalid && !errors.Is(err, ErrInvalidOrder):
				t.Fatalf("err = %v, want ErrInvalidOrder", err)
			case tc.wantUnknownSKU != "" && (!errors.As(err, &unknown) || unknown.SKU != tc.wantUnknownSKU):
				t.Fatalf("err = %v, want UnknownSKUError(%s)", err, tc.wantUnknownSKU)
			}
		})
	}
}

func TestSameRequest(t *testing.T) {
	o := newOrder(t)
	same, _ := NormalizeLines([]Line{{"B", 4}, {"A", 2}})

	if !o.SameRequest("c-1", same) {
		t.Error("equal requests must match regardless of line order")
	}
	if o.SameRequest("c-2", same) {
		t.Error("different customer must not match")
	}
	for name, lines := range map[string][]Line{
		"different quantity": {{"A", 3}, {"B", 4}},
		"different sku":      {{"A", 2}, {"C", 4}},
		"fewer lines":        {{"A", 2}},
	} {
		if o.SameRequest("c-1", lines) {
			t.Errorf("%s must not match", name)
		}
	}
	if got := o.Lines(); !reflect.DeepEqual(got, []Line{{"A", 2}, {"B", 4}}) {
		t.Errorf("Lines = %v", got)
	}
}

func TestHappyPathTransitions(t *testing.T) {
	later := now.Add(time.Minute)
	o := newOrder(t)

	o, err := o.StockReserved(later)
	if err != nil || o.Status != StatusStockReserved || !o.UpdatedAt.Equal(later) {
		t.Fatalf("after StockReserved: %+v, %v", o, err)
	}
	o, err = o.Confirm(later)
	if err != nil || o.Status != StatusConfirmed || !o.Status.Final() {
		t.Fatalf("after Confirm: %+v, %v", o, err)
	}
}

func TestIllegalTransitions(t *testing.T) {
	pending := newOrder(t)
	reserved, _ := pending.StockReserved(now)
	confirmed, _ := reserved.Confirm(now)
	cancelled, _ := pending.Cancel("x", now)

	tests := []struct {
		name string
		do   func() error
	}{
		{"confirm a pending order", func() error { _, err := pending.Confirm(now); return err }},
		{"reserve twice", func() error { _, err := reserved.StockReserved(now); return err }},
		{"reserve a confirmed order", func() error { _, err := confirmed.StockReserved(now); return err }},
		{"reserve a cancelled order", func() error { _, err := cancelled.StockReserved(now); return err }},
		{"confirm a cancelled order", func() error { _, err := cancelled.Confirm(now); return err }},
		{"cancel a cancelled order", func() error { _, err := cancelled.Cancel("again", now); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("err = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestCancelRules(t *testing.T) {
	pending := newOrder(t)
	reserved, _ := pending.StockReserved(now)
	confirmed, _ := reserved.Confirm(now)

	for name, o := range map[string]Order{"pending": pending, "stock reserved": reserved} {
		got, err := o.Cancel("changed my mind", now.Add(time.Second))
		if err != nil || got.Status != StatusCancelled || got.CancelReason != "changed my mind" || !got.Status.Final() {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if _, err := confirmed.Cancel("too late", now); !errors.Is(err, ErrCannotCancel) {
		t.Errorf("cancelling a confirmed order: err = %v, want ErrCannotCancel", err)
	}
}

func TestTransitionsDoNotMutateTheReceiver(t *testing.T) {
	o := newOrder(t)
	_, _ = o.StockReserved(now)
	_, _ = o.Cancel("x", now)
	if o.Status != StatusPending || o.CancelReason != "" {
		t.Fatalf("receiver changed: %+v", o)
	}
}

func TestMarkRefundRequired(t *testing.T) {
	o, _ := newOrder(t).Cancel("x", now)
	got := o.MarkRefundRequired(now.Add(time.Hour))
	if !got.RefundRequired || o.RefundRequired {
		t.Fatalf("got %v, original %v", got.RefundRequired, o.RefundRequired)
	}
}

func TestCancelCauseGroupsFreeTextIntoAFixedSet(t *testing.T) {
	tests := map[string]string{
		"payment failed: card declined":    "payment_failed",
		"out of stock: insufficient stock": "out_of_stock",
		"saga timeout":                     "timeout",
		"cancelled by customer":            "customer",
		"changed my mind":                  "customer",
		"":                                 "customer",
		"a very long free text typed by a user " + strings.Repeat("x", 500): "customer", // never leaks into a label
	}
	for reason, want := range tests {
		if got := CancelCause(reason); got != want {
			t.Errorf("CancelCause(%.40q) = %q, want %q", reason, got, want)
		}
	}
}

func TestUnknownSKUErrorMessage(t *testing.T) {
	if got := (&UnknownSKUError{SKU: "X"}).Error(); got != "unknown SKU X" {
		t.Fatalf("message = %q", got)
	}
}
