// Package domain holds the inventory entities and business rules.
// It must not import adapters, frameworks or I/O libraries.
package domain

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

var (
	// ErrItemNotFound is returned when a SKU is unknown.
	ErrItemNotFound = errors.New("item not found")
	// ErrInvalidQuantity is returned for zero or negative quantities.
	ErrInvalidQuantity = errors.New("quantity must be positive")
	// ErrInvalidReservation is returned for a reservation without items or without an order ID.
	ErrInvalidReservation = errors.New("invalid reservation")
	// ErrReservationNotFound is returned when no reservation exists for an order.
	ErrReservationNotFound = errors.New("reservation not found")
)

// MaxQuantity is the largest quantity of one SKU in a single reservation. It
// matches the INTEGER columns of the database.
const MaxQuantity = math.MaxInt32

// Money is an amount in minor units (e.g. cents).
type Money struct {
	CurrencyCode string
	AmountMinor  int64
}

// Item is a stocked product.
type Item struct {
	SKU       string
	Name      string
	UnitPrice Money
	// OnHand is the physical quantity in the warehouse.
	OnHand int
	// Reserved is the part of OnHand promised to pending orders.
	Reserved int
}

// Available is the quantity that can still be reserved.
func (i Item) Available() int {
	return i.OnHand - i.Reserved
}

// Reserve returns a copy of the item with qty more units reserved.
func (i Item) Reserve(qty int) (Item, error) {
	if qty <= 0 {
		return i, ErrInvalidQuantity
	}
	if qty > i.Available() {
		return i, &InsufficientStockError{SKUs: []string{i.SKU}}
	}
	i.Reserved += qty
	return i, nil
}

// Release returns a copy of the item with qty reserved units given back.
// Releasing more than is reserved is a data-integrity bug, not a user error.
func (i Item) Release(qty int) (Item, error) {
	if qty <= 0 {
		return i, ErrInvalidQuantity
	}
	if qty > i.Reserved {
		return i, fmt.Errorf("release %d of %s but only %d reserved", qty, i.SKU, i.Reserved)
	}
	i.Reserved -= qty
	return i, nil
}

// InsufficientStockError lists the SKUs that could not be satisfied.
type InsufficientStockError struct {
	SKUs []string
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient stock for %v", e.SKUs)
}

// ReservationStatus is the lifecycle state of a reservation.
type ReservationStatus string

const (
	ReservationActive   ReservationStatus = "active"
	ReservationReleased ReservationStatus = "released"
)

// Line is a quantity of one SKU.
type Line struct {
	SKU      string
	Quantity int
}

// Reservation is stock held for one order. There is at most one per order,
// which makes reserving idempotent per order.
type Reservation struct {
	OrderID string
	Lines   []Line
	Status  ReservationStatus
}

// NewReservation validates and normalises a reservation request: lines for
// the same SKU are merged and sorted by SKU, which gives a stable lock order.
func NewReservation(orderID string, lines []Line) (Reservation, error) {
	if orderID == "" {
		return Reservation{}, fmt.Errorf("%w: order ID is required", ErrInvalidReservation)
	}
	if len(lines) == 0 {
		return Reservation{}, fmt.Errorf("%w: at least one line is required", ErrInvalidReservation)
	}

	merged := make(map[string]int, len(lines))
	for _, l := range lines {
		if l.SKU == "" {
			return Reservation{}, fmt.Errorf("%w: SKU is required", ErrInvalidReservation)
		}
		if l.Quantity <= 0 || l.Quantity > MaxQuantity {
			return Reservation{}, fmt.Errorf("%w: %s: %w", ErrInvalidReservation, l.SKU, ErrInvalidQuantity)
		}
		merged[l.SKU] += l.Quantity
		if merged[l.SKU] > MaxQuantity {
			return Reservation{}, fmt.Errorf("%w: %s: total quantity exceeds %d", ErrInvalidReservation, l.SKU, MaxQuantity)
		}
	}

	out := make([]Line, 0, len(merged))
	for sku, qty := range merged {
		out = append(out, Line{SKU: sku, Quantity: qty})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].SKU < out[b].SKU })

	return Reservation{OrderID: orderID, Lines: out, Status: ReservationActive}, nil
}

// SKUs returns the reservation's SKUs in sorted order.
func (r Reservation) SKUs() []string {
	skus := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		skus[i] = l.SKU
	}
	return skus
}

// Reserve applies the reservation to items (keyed by SKU) all-or-nothing and
// returns the updated items. If any SKU is unknown or short, nothing is
// changed and an *InsufficientStockError listing every problem SKU is returned.
func (r Reservation) Reserve(items map[string]Item) (map[string]Item, error) {
	var short []string
	updated := make(map[string]Item, len(r.Lines))
	for _, l := range r.Lines {
		item, ok := items[l.SKU]
		if !ok {
			short = append(short, l.SKU)
			continue
		}
		next, err := item.Reserve(l.Quantity)
		if err != nil {
			short = append(short, l.SKU)
			continue
		}
		updated[l.SKU] = next
	}
	if len(short) > 0 {
		return nil, &InsufficientStockError{SKUs: short}
	}
	return updated, nil
}

// Release undoes the reservation on items and returns the updated items.
func (r Reservation) Release(items map[string]Item) (map[string]Item, error) {
	updated := make(map[string]Item, len(r.Lines))
	for _, l := range r.Lines {
		item, ok := items[l.SKU]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrItemNotFound, l.SKU)
		}
		next, err := item.Release(l.Quantity)
		if err != nil {
			return nil, err
		}
		updated[l.SKU] = next
	}
	return updated, nil
}
