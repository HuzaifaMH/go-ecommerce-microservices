// Package simulated is a fake payment provider for local development, demos
// and tests. It lets you trigger each outcome the saga must handle by
// choosing the customer ID or the amount:
//
//	customer starts with "decline-"  -> the charge is declined
//	customer starts with "flaky-"    -> the first attempt per order fails as a provider outage, then succeeds
//	amount above the configured limit -> the charge is declined
//	anything else                    -> the charge succeeds
//
// Like a real provider with idempotency keys, it returns the same result for
// the same order every time.
package simulated

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

// Customer ID prefixes that select a behaviour.
const (
	DeclinePrefix = "decline-"
	FlakyPrefix   = "flaky-"
)

var _ app.Provider = (*Provider)(nil)

// Provider is the simulated provider.
type Provider struct {
	maxAmountMinor int64

	mu       sync.Mutex
	attempts map[string]int // order ID -> attempts so far
}

// New returns a provider that declines amounts above maxAmountMinor.
func New(maxAmountMinor int64) *Provider {
	return &Provider{maxAmountMinor: maxAmountMinor, attempts: map[string]int{}}
}

// Charge implements app.Provider.
func (p *Provider) Charge(ctx context.Context, req domain.ChargeRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	switch {
	case strings.HasPrefix(req.CustomerID, DeclinePrefix):
		return "", &domain.DeclinedError{Reason: "card declined"}
	case req.Amount.AmountMinor > p.maxAmountMinor:
		return "", &domain.DeclinedError{Reason: "amount exceeds limit"}
	case strings.HasPrefix(req.CustomerID, FlakyPrefix) && p.attempt(req.OrderID) == 1:
		return "", errors.New("provider timeout")
	}
	return "sim_" + req.OrderID, nil
}

func (p *Provider) attempt(orderID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts[orderID]++
	return p.attempts[orderID]
}
