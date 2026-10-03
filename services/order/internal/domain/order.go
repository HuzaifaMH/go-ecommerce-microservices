// Package domain holds the order entity and its state machine.
// It must not import adapters, frameworks or I/O libraries.
package domain

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalidOrder is returned for a malformed order request.
	ErrInvalidOrder = errors.New("invalid order")
	// ErrOrderNotFound is returned when an order does not exist.
	ErrOrderNotFound = errors.New("order not found")
	// ErrCannotCancel is returned when an order is past the point where it can be cancelled.
	ErrCannotCancel = errors.New("order can no longer be cancelled")
	// ErrInvalidTransition is returned when a saga event contradicts the order's state.
	// It indicates a bug or corrupted data, not a condition retrying can fix.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrIdempotencyConflict is returned when an idempotency key is reused for a different request.
	ErrIdempotencyConflict = errors.New("idempotency key was used with different parameters")
)

// MaxQuantity is the largest quantity of one SKU on an order. It matches the
// inventory service's limit.
const MaxQuantity = math.MaxInt32

// UnknownSKUError is returned when a SKU is not in the catalog.
type UnknownSKUError struct{ SKU string }

func (e *UnknownSKUError) Error() string { return "unknown SKU " + e.SKU }

// Money is an amount in minor units (e.g. cents).
type Money struct {
	CurrencyCode string
	AmountMinor  int64
}

// Status is the order's position in the saga.
type Status string

const (
	// StatusPending: accepted; waiting for inventory to reserve stock.
	StatusPending Status = "pending"
	// StatusStockReserved: stock is held; waiting for the payment result.
	StatusStockReserved Status = "stock_reserved"
	// StatusConfirmed: stock reserved and paid. Final.
	StatusConfirmed Status = "confirmed"
	// StatusCancelled: rolled back, cancelled by the customer, or timed out. Final.
	StatusCancelled Status = "cancelled"
)

// Final reports whether no further transitions are possible.
func (s Status) Final() bool { return s == StatusConfirmed || s == StatusCancelled }

// Line is a requested quantity of one SKU, before pricing.
type Line struct {
	SKU      string
	Quantity int
}

// Item is an ordered line with the unit price at the time of ordering.
type Item struct {
	SKU       string
	Quantity  int
	UnitPrice Money
}

// NormalizeLines validates requested lines and merges duplicates. The result
// is sorted by SKU so equal requests compare equal.
func NormalizeLines(lines []Line) ([]Line, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: at least one item is required", ErrInvalidOrder)
	}
	merged := make(map[string]int, len(lines))
	for _, l := range lines {
		if l.SKU == "" {
			return nil, fmt.Errorf("%w: SKU is required", ErrInvalidOrder)
		}
		if l.Quantity <= 0 || l.Quantity > MaxQuantity {
			return nil, fmt.Errorf("%w: %s: quantity must be between 1 and %d", ErrInvalidOrder, l.SKU, MaxQuantity)
		}
		merged[l.SKU] += l.Quantity
		if merged[l.SKU] > MaxQuantity {
			return nil, fmt.Errorf("%w: %s: total quantity exceeds %d", ErrInvalidOrder, l.SKU, MaxQuantity)
		}
	}
	out := make([]Line, 0, len(merged))
	for sku, q := range merged {
		out = append(out, Line{SKU: sku, Quantity: q})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].SKU < out[b].SKU })
	return out, nil
}

// Order is a customer order and the saga state that tracks it.
type Order struct {
	ID             string
	CustomerID     string
	IdempotencyKey string
	Items          []Item
	Total          Money
	Status         Status
	// CancelReason is set when Status is StatusCancelled.
	CancelReason string
	// RefundRequired is set when a payment succeeded for an order that was
	// already cancelled (a rare race). It marks money that must be returned.
	RefundRequired bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewOrder creates a pending order, pricing each line from prices (SKU -> unit
// price). All prices must share one currency. The total is computed with
// overflow checks.
func NewOrder(id, customerID, idempotencyKey string, lines []Line, prices map[string]Money, now time.Time) (Order, error) {
	if id == "" {
		return Order{}, fmt.Errorf("%w: order ID is required", ErrInvalidOrder)
	}
	if customerID == "" {
		return Order{}, fmt.Errorf("%w: customer ID is required", ErrInvalidOrder)
	}
	if idempotencyKey == "" {
		return Order{}, fmt.Errorf("%w: idempotency key is required", ErrInvalidOrder)
	}
	lines, err := NormalizeLines(lines)
	if err != nil {
		return Order{}, err
	}

	items := make([]Item, 0, len(lines))
	var total Money
	for _, l := range lines {
		price, ok := prices[l.SKU]
		if !ok {
			return Order{}, &UnknownSKUError{SKU: l.SKU}
		}
		if price.AmountMinor < 0 {
			return Order{}, fmt.Errorf("%w: %s has a negative price", ErrInvalidOrder, l.SKU)
		}
		if total.CurrencyCode == "" {
			total.CurrencyCode = price.CurrencyCode
		} else if total.CurrencyCode != price.CurrencyCode {
			return Order{}, fmt.Errorf("%w: items are priced in different currencies", ErrInvalidOrder)
		}

		lineTotal, ok := mulCheck(price.AmountMinor, int64(l.Quantity))
		if !ok {
			return Order{}, fmt.Errorf("%w: total is too large", ErrInvalidOrder)
		}
		if total.AmountMinor > math.MaxInt64-lineTotal {
			return Order{}, fmt.Errorf("%w: total is too large", ErrInvalidOrder)
		}
		total.AmountMinor += lineTotal
		items = append(items, Item{SKU: l.SKU, Quantity: l.Quantity, UnitPrice: price})
	}
	if total.AmountMinor <= 0 {
		return Order{}, fmt.Errorf("%w: order total must be positive", ErrInvalidOrder)
	}

	return Order{
		ID: id, CustomerID: customerID, IdempotencyKey: idempotencyKey,
		Items: items, Total: total, Status: StatusPending,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func mulCheck(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

// Lines returns the order's items as unpriced lines.
func (o Order) Lines() []Line {
	out := make([]Line, len(o.Items))
	for i, it := range o.Items {
		out[i] = Line{SKU: it.SKU, Quantity: it.Quantity}
	}
	return out
}

// SameRequest reports whether the order was created from this customer and
// these (normalised) lines. Used to detect an idempotency key reused for a
// different request.
func (o Order) SameRequest(customerID string, lines []Line) bool {
	if o.CustomerID != customerID || len(o.Items) != len(lines) {
		return false
	}
	for i, it := range o.Items {
		if it.SKU != lines[i].SKU || it.Quantity != lines[i].Quantity {
			return false
		}
	}
	return true
}

// StockReserved moves a pending order to stock_reserved.
func (o Order) StockReserved(now time.Time) (Order, error) {
	if o.Status != StatusPending {
		return o, fmt.Errorf("%w: stock reserved while %s", ErrInvalidTransition, o.Status)
	}
	o.Status, o.UpdatedAt = StatusStockReserved, now
	return o, nil
}

// Confirm moves an order whose stock is reserved to confirmed (payment succeeded).
func (o Order) Confirm(now time.Time) (Order, error) {
	if o.Status != StatusStockReserved {
		return o, fmt.Errorf("%w: confirm while %s", ErrInvalidTransition, o.Status)
	}
	o.Status, o.UpdatedAt = StatusConfirmed, now
	return o, nil
}

// Cancel cancels an order that is pending or has its stock reserved. A
// confirmed order cannot be cancelled (that would need a refund flow).
func (o Order) Cancel(reason string, now time.Time) (Order, error) {
	switch o.Status {
	case StatusPending, StatusStockReserved:
		o.Status, o.CancelReason, o.UpdatedAt = StatusCancelled, reason, now
		return o, nil
	case StatusConfirmed:
		return o, ErrCannotCancel
	default:
		return o, fmt.Errorf("%w: cancel while %s", ErrInvalidTransition, o.Status)
	}
}

// CancelCause groups a cancellation reason into a small fixed set of causes
// ("payment_failed", "out_of_stock", "timeout", "customer"), for metrics. A
// reason is free text, so it must never be used as a metric label directly.
func CancelCause(reason string) string {
	switch {
	case strings.HasPrefix(reason, "payment failed"):
		return "payment_failed"
	case strings.HasPrefix(reason, "out of stock"):
		return "out_of_stock"
	case reason == "saga timeout":
		return "timeout"
	default:
		return "customer"
	}
}

// MarkRefundRequired flags a payment that succeeded for a cancelled order.
func (o Order) MarkRefundRequired(now time.Time) Order {
	o.RefundRequired, o.UpdatedAt = true, now
	return o
}
