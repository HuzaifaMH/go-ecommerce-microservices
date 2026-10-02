// Package simulated is a fake email/SMS sender for local development, demos
// and tests. It "delivers" by writing a log line, and lets you trigger the
// failure modes the service must handle by choosing the recipient (which is
// the customer ID):
//
//	recipient starts with "bounce-" -> permanently undeliverable
//	recipient starts with "flaky-"  -> the first attempt per notification fails (gateway timeout), then succeeds
//	anything else                   -> delivered
package simulated

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

// Recipient prefixes that select a behaviour.
const (
	BouncePrefix = "bounce-"
	FlakyPrefix  = "flaky-"
)

var _ app.Sender = (*Sender)(nil)

// Sender is the simulated sender.
type Sender struct {
	log *slog.Logger

	mu       sync.Mutex
	attempts map[string]int // notification ID -> attempts so far
}

// New returns a Sender that logs deliveries to log.
func New(log *slog.Logger) *Sender {
	return &Sender{log: log, attempts: map[string]int{}}
}

// Send implements app.Sender.
func (s *Sender) Send(ctx context.Context, n domain.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	switch {
	case strings.HasPrefix(n.Recipient, BouncePrefix):
		return &domain.UndeliverableError{Reason: unreachable(n.Channel)}
	case strings.HasPrefix(n.Recipient, FlakyPrefix) && s.attempt(n.ID) == 1:
		return errors.New("gateway timeout")
	}

	s.log.Info("notification sent",
		"channel", n.Channel, "recipient", n.Recipient, "subject", n.Subject,
		"order_id", n.OrderID, "notification_id", n.ID)
	return nil
}

func (s *Sender) attempt(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[id]++
	return s.attempts[id]
}

func unreachable(c domain.Channel) string {
	if c == domain.ChannelSMS {
		return "number not reachable"
	}
	return "mailbox does not exist"
}
