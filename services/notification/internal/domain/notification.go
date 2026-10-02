// Package domain holds the notification entity and the rules for turning order
// outcomes into messages.
// It must not import adapters, frameworks or I/O libraries.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidEvent is returned for an order event that lacks the data needed to
// notify anyone. Retrying cannot fix it.
var ErrInvalidEvent = errors.New("invalid event")

// UndeliverableError is returned by a sender when a notification can never be
// delivered (for example the address does not exist). Unlike a transient
// failure it is not retried.
type UndeliverableError struct {
	Reason string
}

func (e *UndeliverableError) Error() string { return "undeliverable: " + e.Reason }

// Kind says what a notification is about.
type Kind string

const (
	KindOrderConfirmed Kind = "order_confirmed"
	KindOrderCancelled Kind = "order_cancelled"
)

// Channel is how a notification is delivered.
type Channel string

const (
	ChannelEmail Channel = "email"
	ChannelSMS   Channel = "sms"
)

// Status is the delivery state of a notification.
type Status string

const (
	// StatusPending: recorded, not delivered yet (or being retried).
	StatusPending Status = "pending"
	// StatusSent: delivered. Final.
	StatusSent Status = "sent"
	// StatusFailed: permanently undeliverable. Final.
	StatusFailed Status = "failed"
)

// Money is an amount in minor units with two decimals (e.g. cents).
type Money struct {
	CurrencyCode string
	AmountMinor  int64
}

// String formats the amount for people, e.g. "19.99 USD". It assumes a
// currency with two minor-unit digits.
func (m Money) String() string {
	amount := m.AmountMinor
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, amount/100, amount%100, m.CurrencyCode)
}

// Notification is one message to one recipient on one channel.
//
// There is at most one per (OrderID, Kind, Channel). That uniqueness is what
// stops a redelivered order event from notifying the customer twice.
type Notification struct {
	ID         string
	OrderID    string
	CustomerID string
	Kind       Kind
	Channel    Channel
	// Recipient is where the message goes. The customer ID is used as the
	// address for now: events carry no contact details.
	Recipient     string
	Subject       string
	Body          string
	Status        Status
	FailureReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// OrderConfirmed builds the notifications for a confirmed order: an email.
func OrderConfirmed(newID func() string, orderID, customerID string, total Money, now time.Time) ([]Notification, error) {
	if err := validate(orderID, customerID); err != nil {
		return nil, err
	}
	subject := "Your order is confirmed"
	body := fmt.Sprintf("Thank you! Your order %s for %s is confirmed and will be on its way soon.", orderID, total)

	return []Notification{
		newNotification(newID, orderID, customerID, KindOrderConfirmed, ChannelEmail, subject, body, now),
	}, nil
}

// OrderCancelled builds the notifications for a cancelled order: an email with
// the explanation, and an SMS because the customer may need to act.
func OrderCancelled(newID func() string, orderID, customerID, reason string, now time.Time) ([]Notification, error) {
	if err := validate(orderID, customerID); err != nil {
		return nil, err
	}
	subject := "Your order was cancelled"
	explanation := Explain(reason)

	return []Notification{
		newNotification(newID, orderID, customerID, KindOrderCancelled, ChannelEmail, subject,
			fmt.Sprintf("Your order %s was cancelled. %s", orderID, explanation), now),
		newNotification(newID, orderID, customerID, KindOrderCancelled, ChannelSMS, subject,
			fmt.Sprintf("Order %s cancelled. %s", orderID, explanation), now),
	}, nil
}

// Explain turns the order service's cancellation reason into a sentence for a
// customer. Reasons are matched by prefix; anything unrecognised, such as a
// reason typed by the customer, is quoted as is.
func Explain(reason string) string {
	switch {
	case strings.HasPrefix(reason, "payment failed"):
		return "We could not process your payment. Please check your payment method and place the order again."
	case strings.HasPrefix(reason, "out of stock"):
		return "Some items are no longer available. You have not been charged."
	case reason == "saga timeout":
		return "It took too long to process. If you see a charge for this order, please contact support."
	case reason == "":
		return "No reason was given."
	default:
		return "Reason: " + reason + "."
	}
}

func validate(orderID, customerID string) error {
	if orderID == "" {
		return fmt.Errorf("%w: order ID is required", ErrInvalidEvent)
	}
	if customerID == "" {
		return fmt.Errorf("%w: customer ID is required", ErrInvalidEvent)
	}
	return nil
}

func newNotification(newID func() string, orderID, customerID string, kind Kind, ch Channel, subject, body string, now time.Time) Notification {
	return Notification{
		ID: newID(), OrderID: orderID, CustomerID: customerID, Kind: kind, Channel: ch,
		Recipient: customerID, Subject: subject, Body: body,
		Status: StatusPending, CreatedAt: now, UpdatedAt: now,
	}
}
