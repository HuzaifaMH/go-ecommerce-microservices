package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

const consumerName = "order"

// Saga is the part of the application that reacts to replies from the other services.
type Saga interface {
	HandleStockReserved(ctx context.Context, orderID string) error
	HandleStockRejected(ctx context.Context, orderID, reason string) error
	HandlePaymentSucceeded(ctx context.Context, orderID string) error
	HandlePaymentFailed(ctx context.Context, orderID, reason string) error
}

// Handlers decodes saga replies and calls the application.
type Handlers struct {
	saga Saga
}

// NewHandlers returns the reply handlers.
func NewHandlers(saga Saga) *Handlers {
	return &Handlers{saga: saga}
}

// Handle routes a reply by subject. Messages that can never succeed (garbage
// payloads, replies for unknown orders, contradictory states) are permanent
// failures; everything else is retried.
func (h *Handlers) Handle(ctx context.Context, m messaging.Message) error {
	switch m.Subject {
	case subjects.InventoryEvtReserved:
		var e inventoryv1.StockReserved
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		return classify(h.saga.HandleStockReserved(ctx, e.GetOrderId()))

	case subjects.InventoryEvtRejected:
		var e inventoryv1.StockRejected
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		return classify(h.saga.HandleStockRejected(ctx, e.GetOrderId(), e.GetReason()))

	case subjects.PaymentEvtSucceeded:
		var e paymentv1.PaymentSucceeded
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		return classify(h.saga.HandlePaymentSucceeded(ctx, e.GetOrderId()))

	case subjects.PaymentEvtFailed:
		var e paymentv1.PaymentFailed
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		return classify(h.saga.HandlePaymentFailed(ctx, e.GetOrderId(), e.GetReason()))

	default:
		return permanent(fmt.Errorf("unexpected subject %q", m.Subject))
	}
}

func undecodable(subject string, err error) error {
	return permanent(fmt.Errorf("decode %s: %w", subject, err))
}

// classify marks errors that retrying cannot fix as permanent.
func classify(err error) error {
	if errors.Is(err, domain.ErrOrderNotFound) || errors.Is(err, domain.ErrInvalidTransition) {
		return permanent(err)
	}
	return err
}
func permanent(err error) error {
	return fmt.Errorf("%w: %w", messaging.ErrPermanent, err)
}

// Consume runs the consumers for inventory and payment replies until ctx is
// cancelled. Each reply goes through the inbox, so redelivery never advances
// the saga twice.
func Consume(ctx context.Context, js jetstream.JetStream, inbox messaging.Inbox, h *Handlers, log *slog.Logger, obs messaging.Observer) error {
	handler := messaging.Idempotent(consumerName, inbox, h.Handle)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return messaging.Consume(ctx, js, messaging.ConsumerConfig{
			Stream: subjects.InventoryStream.Name, Durable: consumerName,
			FilterSubjects: []string{subjects.InventoryEvtReserved, subjects.InventoryEvtRejected},
			Observer:       obs,
		}, log, handler)
	})
	g.Go(func() error {
		return messaging.Consume(ctx, js, messaging.ConsumerConfig{
			Stream: subjects.PaymentStream.Name, Durable: consumerName,
			FilterSubjects: []string{subjects.PaymentEvtSucceeded, subjects.PaymentEvtFailed},
			Observer:       obs,
		}, log, handler)
	})
	return g.Wait()
}

// Enqueuer adds a message to the transactional outbox of the current transaction.
type Enqueuer interface {
	Enqueue(ctx context.Context, m messaging.Message) (string, error)
}

var (
	_ app.Commands = (*Publisher)(nil)
	_ app.Events   = (*Publisher)(nil)
)

// Publisher sends saga commands and announces order outcomes by writing them
// to the outbox, so they are published if and only if the business
// transaction commits.
type Publisher struct {
	outbox Enqueuer
}

// NewPublisher returns the command and event publisher.
func NewPublisher(outbox Enqueuer) *Publisher {
	return &Publisher{outbox: outbox}
}

func (p *Publisher) ReserveStock(ctx context.Context, orderID string, lines []domain.Line) error {
	items := make([]*commonv1.LineItem, len(lines))
	for i, l := range lines {
		items[i] = &commonv1.LineItem{Sku: l.SKU, Quantity: int32(l.Quantity)} //nolint:gosec // bounded by domain.MaxQuantity
	}
	return p.enqueue(ctx, orderID, subjects.InventoryCmdReserve, &inventoryv1.ReserveStock{OrderId: orderID, Items: items})
}

func (p *Publisher) ReleaseStock(ctx context.Context, orderID string) error {
	return p.enqueue(ctx, orderID, subjects.InventoryCmdRelease, &inventoryv1.ReleaseStock{OrderId: orderID})
}

func (p *Publisher) ChargePayment(ctx context.Context, orderID, customerID string, total domain.Money) error {
	return p.enqueue(ctx, orderID, subjects.PaymentCmdCharge, &paymentv1.ChargePayment{
		OrderId: orderID, CustomerId: customerID,
		Amount: &commonv1.Money{CurrencyCode: total.CurrencyCode, AmountMinor: total.AmountMinor},
	})
}

func (p *Publisher) OrderConfirmed(ctx context.Context, o domain.Order) error {
	return p.enqueue(ctx, o.ID, subjects.OrderEvtConfirmed, &orderv1.OrderConfirmed{
		OrderId: o.ID, CustomerId: o.CustomerID,
		Total: &commonv1.Money{CurrencyCode: o.Total.CurrencyCode, AmountMinor: o.Total.AmountMinor},
	})
}

func (p *Publisher) OrderCancelled(ctx context.Context, o domain.Order) error {
	return p.enqueue(ctx, o.ID, subjects.OrderEvtCancelled, &orderv1.OrderCancelled{
		OrderId: o.ID, CustomerId: o.CustomerID, Reason: o.CancelReason,
	})
}

// enqueue writes the message to the outbox. A message sent outside a reply
// handler has no correlation ID yet, so the order ID becomes the correlation
// ID that ties every message of one order's saga together.
func (p *Publisher) enqueue(ctx context.Context, orderID, subject string, payload proto.Message) error {
	if messaging.CorrelationID(ctx) == "" {
		ctx = messaging.WithCorrelationID(ctx, orderID)
	}
	msg, err := messaging.NewProtoMessage(ctx, subject, payload)
	if err != nil {
		return err
	}
	if _, err := p.outbox.Enqueue(ctx, msg); err != nil {
		return fmt.Errorf("enqueue %s: %w", subject, err)
	}
	return nil
}
