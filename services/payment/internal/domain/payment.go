// Package domain holds the payment entities and business rules.
// It must not import adapters, frameworks or I/O libraries.
package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidPayment is returned for a malformed or conflicting charge request.
	// Retrying cannot fix it.
	ErrInvalidPayment = errors.New("invalid payment")
	// ErrPaymentNotFound is returned when an order has no payment.
	ErrPaymentNotFound = errors.New("payment not found")
)

// DeclinedError is returned by a payment provider when it refuses a charge
// (insufficient funds, blocked card, ...). It is a business outcome, unlike a
// provider outage, which is a plain error and can be retried.
type DeclinedError struct {
	Reason string
}

func (e *DeclinedError) Error() string { return "payment declined: " + e.Reason }

// Money is an amount in minor units (e.g. cents).
type Money struct {
	CurrencyCode string
	AmountMinor  int64
}

// Status is the outcome of a charge.
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// ChargeRequest is a validated request to charge a customer for an order.
type ChargeRequest struct {
	OrderID    string
	CustomerID string
	Amount     Money
}

// NewChargeRequest validates the inputs of a charge.
func NewChargeRequest(orderID, customerID string, amount Money) (ChargeRequest, error) {
	switch {
	case orderID == "":
		return ChargeRequest{}, fmt.Errorf("%w: order ID is required", ErrInvalidPayment)
	case customerID == "":
		return ChargeRequest{}, fmt.Errorf("%w: customer ID is required", ErrInvalidPayment)
	case !validCurrency(amount.CurrencyCode):
		return ChargeRequest{}, fmt.Errorf("%w: currency must be a 3-letter upper-case ISO 4217 code, got %q", ErrInvalidPayment, amount.CurrencyCode)
	case amount.AmountMinor <= 0:
		return ChargeRequest{}, fmt.Errorf("%w: amount must be positive", ErrInvalidPayment)
	}
	return ChargeRequest{OrderID: orderID, CustomerID: customerID, Amount: amount}, nil
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// Payment is the recorded outcome of charging an order. There is at most one
// per order, which makes charging idempotent.
type Payment struct {
	ID            string
	OrderID       string
	CustomerID    string
	Amount        Money
	Status        Status
	FailureReason string
	ProviderRef   string
	CreatedAt     time.Time
}

// NewSucceeded records a successful charge.
func NewSucceeded(id string, req ChargeRequest, providerRef string) Payment {
	return Payment{ID: id, OrderID: req.OrderID, CustomerID: req.CustomerID, Amount: req.Amount,
		Status: StatusSucceeded, ProviderRef: providerRef}
}

// NewFailed records a declined charge.
func NewFailed(id string, req ChargeRequest, reason string) Payment {
	return Payment{ID: id, OrderID: req.OrderID, CustomerID: req.CustomerID, Amount: req.Amount,
		Status: StatusFailed, FailureReason: reason}
}

// Matches reports whether the payment was made for exactly this request.
func (p Payment) Matches(req ChargeRequest) bool {
	return p.OrderID == req.OrderID && p.CustomerID == req.CustomerID && p.Amount == req.Amount
}
