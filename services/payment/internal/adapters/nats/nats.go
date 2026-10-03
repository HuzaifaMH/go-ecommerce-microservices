package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

const consumerName = "payment"

// Commands is the part of the application the command handler calls.
type Commands interface {
	Charge(ctx context.Context, orderID, customerID string, amount domain.Money) error
}

// Handlers decodes saga commands and calls the application.
type Handlers struct {
	cmds Commands
}

// NewHandlers returns the command handlers.
func NewHandlers(cmds Commands) *Handlers {
	return &Handlers{cmds: cmds}
}

// Handle processes a ChargePayment command. Malformed commands are permanent
// failures so the broker does not redeliver them forever; provider outages
// are returned as ordinary errors and retried.
func (h *Handlers) Handle(ctx context.Context, m messaging.Message) error {
	if m.Subject != subjects.PaymentCmdCharge {
		return permanent(fmt.Errorf("unexpected subject %q", m.Subject))
	}
	var cmd paymentv1.ChargePayment
	if err := proto.Unmarshal(m.Data, &cmd); err != nil {
		return permanent(fmt.Errorf("decode ChargePayment: %w", err))
	}

	amount := domain.Money{CurrencyCode: cmd.GetAmount().GetCurrencyCode(), AmountMinor: cmd.GetAmount().GetAmountMinor()}
	err := h.cmds.Charge(ctx, cmd.GetOrderId(), cmd.GetCustomerId(), amount)
	if errors.Is(err, domain.ErrInvalidPayment) {
		return permanent(err)
	}
	return err
}

func permanent(err error) error {
	return fmt.Errorf("%w: %w", messaging.ErrPermanent, err)
}

// Consume runs the payment command consumer until ctx is cancelled.
//
// It deliberately does not use the inbox: charging is idempotent per order
// (one payment row per order, idempotency key at the provider), and an inbox
// transaction would stay open during the provider call.
func Consume(ctx context.Context, js jetstream.JetStream, h *Handlers, log *slog.Logger, obs messaging.Observer) error {
	return messaging.Consume(ctx, js, messaging.ConsumerConfig{
		Stream:         subjects.PaymentStream.Name,
		Durable:        consumerName,
		FilterSubjects: []string{subjects.PaymentCmdCharge},
		Observer:       obs,
	}, log, h.Handle)
}

// Enqueuer adds a message to the transactional outbox of the current transaction.
type Enqueuer interface {
	Enqueue(ctx context.Context, m messaging.Message) (string, error)
}

var _ app.Events = (*Events)(nil)

// Events implements app.Events by writing replies to the outbox, so they are
// published if and only if the business transaction commits.
type Events struct {
	outbox Enqueuer
}

// NewEvents returns the event publisher.
func NewEvents(outbox Enqueuer) *Events {
	return &Events{outbox: outbox}
}

func (e *Events) PaymentSucceeded(ctx context.Context, orderID, paymentID string) error {
	return e.enqueue(ctx, subjects.PaymentEvtSucceeded, &paymentv1.PaymentSucceeded{OrderId: orderID, PaymentId: paymentID})
}

func (e *Events) PaymentFailed(ctx context.Context, orderID, reason string) error {
	return e.enqueue(ctx, subjects.PaymentEvtFailed, &paymentv1.PaymentFailed{OrderId: orderID, Reason: reason})
}

func (e *Events) enqueue(ctx context.Context, subject string, payload proto.Message) error {
	msg, err := messaging.NewProtoMessage(ctx, subject, payload)
	if err != nil {
		return err
	}
	if _, err := e.outbox.Enqueue(ctx, msg); err != nil {
		return fmt.Errorf("enqueue %s: %w", subject, err)
	}
	return nil
}
