package nats

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

// fakeSaga records which step was invoked and can fail it.
type fakeSaga struct {
	calls []string
	err   error
}

func (f *fakeSaga) HandleStockReserved(_ context.Context, id string) error {
	f.calls = append(f.calls, "reserved:"+id)
	return f.err
}
func (f *fakeSaga) HandleStockRejected(_ context.Context, id, reason string) error {
	f.calls = append(f.calls, "rejected:"+id+":"+reason)
	return f.err
}
func (f *fakeSaga) HandlePaymentSucceeded(_ context.Context, id string) error {
	f.calls = append(f.calls, "paid:"+id)
	return f.err
}
func (f *fakeSaga) HandlePaymentFailed(_ context.Context, id, reason string) error {
	f.calls = append(f.calls, "payfailed:"+id+":"+reason)
	return f.err
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleRoutesEachReply(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		payload proto.Message
		want    string
	}{
		{"stock reserved", subjects.InventoryEvtReserved, &inventoryv1.StockReserved{OrderId: "o-1"}, "reserved:o-1"},
		{"stock rejected", subjects.InventoryEvtRejected, &inventoryv1.StockRejected{OrderId: "o-1", Reason: "insufficient stock"}, "rejected:o-1:insufficient stock"},
		{"payment succeeded", subjects.PaymentEvtSucceeded, &paymentv1.PaymentSucceeded{OrderId: "o-1", PaymentId: "p-1"}, "paid:o-1"},
		{"payment failed", subjects.PaymentEvtFailed, &paymentv1.PaymentFailed{OrderId: "o-1", Reason: "card declined"}, "payfailed:o-1:card declined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			saga := &fakeSaga{}
			err := NewHandlers(saga).Handle(context.Background(), messaging.Message{ID: "m", Subject: tc.subject, Data: marshal(t, tc.payload)})
			if err != nil || !reflect.DeepEqual(saga.calls, []string{tc.want}) {
				t.Fatalf("err = %v, calls = %v, want [%s]", err, saga.calls, tc.want)
			}
		})
	}
}

func TestHandleClassifiesErrors(t *testing.T) {
	valid := marshal(t, &inventoryv1.StockReserved{OrderId: "o-1"})
	garbage := []byte{0xff, 0xff, 0xff}
	boom := errors.New("connection reset")

	tests := []struct {
		name          string
		msg           messaging.Message
		sagaErr       error
		wantPermanent bool
		wantErr       bool
	}{
		{"garbage in every reply type (reserved)", messaging.Message{Subject: subjects.InventoryEvtReserved, Data: garbage}, nil, true, true},
		{"garbage (rejected)", messaging.Message{Subject: subjects.InventoryEvtRejected, Data: garbage}, nil, true, true},
		{"garbage (paid)", messaging.Message{Subject: subjects.PaymentEvtSucceeded, Data: garbage}, nil, true, true},
		{"garbage (payment failed)", messaging.Message{Subject: subjects.PaymentEvtFailed, Data: garbage}, nil, true, true},
		{"unexpected subject", messaging.Message{Subject: "inventory.evt.released"}, nil, true, true},
		{"unknown order is permanent", messaging.Message{Subject: subjects.InventoryEvtReserved, Data: valid}, domain.ErrOrderNotFound, true, true},
		{"contradictory state is permanent", messaging.Message{Subject: subjects.InventoryEvtReserved, Data: valid}, domain.ErrInvalidTransition, true, true},
		{"database error is retried", messaging.Message{Subject: subjects.InventoryEvtReserved, Data: valid}, boom, false, true},
		{"success", messaging.Message{Subject: subjects.InventoryEvtReserved, Data: valid}, nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHandlers(&fakeSaga{err: tc.sagaErr}).Handle(context.Background(), tc.msg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := errors.Is(err, messaging.ErrPermanent); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err %v)", got, tc.wantPermanent, err)
			}
		})
	}
}

type fakeOutbox struct{ msgs []messaging.Message }

func (f *fakeOutbox) Enqueue(_ context.Context, m messaging.Message) (string, error) {
	f.msgs = append(f.msgs, m)
	return "id", nil
}

func TestPublisherCommands(t *testing.T) {
	ob := &fakeOutbox{}
	p := NewPublisher(ob)
	ctx := context.Background()

	if err := p.ReserveStock(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseStock(ctx, "o-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.ChargePayment(ctx, "o-1", "c-1", domain.Money{CurrencyCode: "USD", AmountMinor: 3000}); err != nil {
		t.Fatal(err)
	}
	if len(ob.msgs) != 3 {
		t.Fatalf("enqueued %d messages", len(ob.msgs))
	}

	var reserve inventoryv1.ReserveStock
	if m := ob.msgs[0]; m.Subject != subjects.InventoryCmdReserve || proto.Unmarshal(m.Data, &reserve) != nil ||
		reserve.GetOrderId() != "o-1" || len(reserve.GetItems()) != 2 || reserve.GetItems()[0].GetSku() != "A" || reserve.GetItems()[0].GetQuantity() != 2 {
		t.Errorf("reserve = %+v / %v", ob.msgs[0], &reserve)
	}
	var release inventoryv1.ReleaseStock
	if m := ob.msgs[1]; m.Subject != subjects.InventoryCmdRelease || proto.Unmarshal(m.Data, &release) != nil || release.GetOrderId() != "o-1" {
		t.Errorf("release = %+v", ob.msgs[1])
	}
	var charge paymentv1.ChargePayment
	if m := ob.msgs[2]; m.Subject != subjects.PaymentCmdCharge || proto.Unmarshal(m.Data, &charge) != nil ||
		charge.GetOrderId() != "o-1" || charge.GetCustomerId() != "c-1" || charge.GetAmount().GetAmountMinor() != 3000 || charge.GetAmount().GetCurrencyCode() != "USD" {
		t.Errorf("charge = %+v / %v", ob.msgs[2], &charge)
	}
}

func TestPublisherEvents(t *testing.T) {
	ob := &fakeOutbox{}
	p := NewPublisher(ob)
	o := domain.Order{ID: "o-1", CustomerID: "c-1", Total: domain.Money{CurrencyCode: "USD", AmountMinor: 3000}, CancelReason: "payment failed: card declined"}

	if err := p.OrderConfirmed(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := p.OrderCancelled(context.Background(), o); err != nil {
		t.Fatal(err)
	}

	var confirmed orderv1.OrderConfirmed
	if m := ob.msgs[0]; m.Subject != subjects.OrderEvtConfirmed || proto.Unmarshal(m.Data, &confirmed) != nil ||
		confirmed.GetOrderId() != "o-1" || confirmed.GetCustomerId() != "c-1" || confirmed.GetTotal().GetAmountMinor() != 3000 {
		t.Errorf("confirmed = %+v / %v", ob.msgs[0], &confirmed)
	}
	var cancelled orderv1.OrderCancelled
	if m := ob.msgs[1]; m.Subject != subjects.OrderEvtCancelled || proto.Unmarshal(m.Data, &cancelled) != nil ||
		cancelled.GetReason() != "payment failed: card declined" {
		t.Errorf("cancelled = %+v / %v", ob.msgs[1], &cancelled)
	}
}

func TestPublisherCorrelationIDDefaultsToOrderID(t *testing.T) {
	ob := &fakeOutbox{}
	p := NewPublisher(ob)

	// Without a correlation ID in ctx (a new order), the order ID is used.
	_ = p.ReleaseStock(context.Background(), "o-7")
	if got := ob.msgs[0].Headers[messaging.HeaderCorrelationID]; got != "o-7" {
		t.Errorf("correlation ID = %q, want the order ID", got)
	}
	// A correlation ID already in ctx (a reply being handled) is kept.
	_ = p.ReleaseStock(messaging.WithCorrelationID(context.Background(), "corr-9"), "o-7")
	if got := ob.msgs[1].Headers[messaging.HeaderCorrelationID]; got != "corr-9" {
		t.Errorf("correlation ID = %q, want corr-9", got)
	}
	if got := ob.msgs[0].Headers[messaging.HeaderMessageType]; got != "ecommerce.inventory.v1.ReleaseStock" {
		t.Errorf("message type = %q", got)
	}
}
