package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

const consumerName = "inventory"

// Commands is the part of the application the command handlers call.
type Commands interface {
	Reserve(ctx context.Context, orderID string, lines []domain.Line) error
	Release(ctx context.Context, orderID string) error
}

// Handlers decodes saga commands and calls the application.
type Handlers struct {
	cmds Commands
}

// NewHandlers returns the command handlers.
func NewHandlers(cmds Commands) *Handlers {
	return &Handlers{cmds: cmds}
}

// Handle routes a command by subject. Malformed messages are reported as
// permanent failures so the broker does not redeliver them forever.
func (h *Handlers) Handle(ctx context.Context, m messaging.Message) error {
	switch m.Subject {
	case subjects.InventoryCmdReserve:
		var cmd inventoryv1.ReserveStock
		if err := proto.Unmarshal(m.Data, &cmd); err != nil {
			return permanent(fmt.Errorf("decode ReserveStock: %w", err))
		}
		lines := make([]domain.Line, len(cmd.GetItems()))
		for i, l := range cmd.GetItems() {
			lines[i] = domain.Line{SKU: l.GetSku(), Quantity: int(l.GetQuantity())}
		}
		return classify(h.cmds.Reserve(ctx, cmd.GetOrderId(), lines))

	case subjects.InventoryCmdRelease:
		var cmd inventoryv1.ReleaseStock
		if err := proto.Unmarshal(m.Data, &cmd); err != nil {
			return permanent(fmt.Errorf("decode ReleaseStock: %w", err))
		}
		return classify(h.cmds.Release(ctx, cmd.GetOrderId()))

	default:
		return permanent(fmt.Errorf("unexpected subject %q", m.Subject))
	}
}

func classify(err error) error {
	if errors.Is(err, domain.ErrInvalidReservation) {
		return permanent(err)
	}
	return err
}

func permanent(err error) error {
	return fmt.Errorf("%w: %w", messaging.ErrPermanent, err)
}

// Consume runs the inventory command consumer until ctx is cancelled. Each
// message is processed through the inbox, so redelivery never reserves twice.
func Consume(ctx context.Context, js jetstream.JetStream, inbox messaging.Inbox, h *Handlers, log *slog.Logger, obs messaging.Observer) error {
	return messaging.Consume(ctx, js, messaging.ConsumerConfig{
		Stream:         subjects.InventoryStream.Name,
		Durable:        consumerName,
		FilterSubjects: []string{subjects.InventoryCmdReserve, subjects.InventoryCmdRelease},
		Observer:       obs,
	}, log, messaging.Idempotent(consumerName, inbox, h.Handle))
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

func (e *Events) StockReserved(ctx context.Context, orderID string) error {
	return e.enqueue(ctx, subjects.InventoryEvtReserved, &inventoryv1.StockReserved{OrderId: orderID})
}

func (e *Events) StockRejected(ctx context.Context, orderID, reason string, unavailableSKUs []string) error {
	return e.enqueue(ctx, subjects.InventoryEvtRejected, &inventoryv1.StockRejected{
		OrderId: orderID, Reason: reason, UnavailableSkus: unavailableSKUs,
	})
}

func (e *Events) StockReleased(ctx context.Context, orderID string) error {
	return e.enqueue(ctx, subjects.InventoryEvtReleased, &inventoryv1.StockReleased{OrderId: orderID})
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
