package nats

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

type fakeCommands struct {
	reserveOrder string
	reserveLines []domain.Line
	releaseOrder string
	err          error
}

func (f *fakeCommands) Reserve(_ context.Context, orderID string, lines []domain.Line) error {
	f.reserveOrder, f.reserveLines = orderID, lines
	return f.err
}

func (f *fakeCommands) Release(_ context.Context, orderID string) error {
	f.releaseOrder = orderID
	return f.err
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleReserveDecodesCommand(t *testing.T) {
	cmds := &fakeCommands{}
	data := mustMarshal(t, &inventoryv1.ReserveStock{
		OrderId: "o-1",
		Items:   []*commonv1.LineItem{{Sku: "A", Quantity: 2}, {Sku: "B", Quantity: 1}},
	})

	err := NewHandlers(cmds).Handle(context.Background(), messaging.Message{ID: "m-1", Subject: subjects.InventoryCmdReserve, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if cmds.reserveOrder != "o-1" || !reflect.DeepEqual(cmds.reserveLines, []domain.Line{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 1}}) {
		t.Fatalf("got order=%q lines=%v", cmds.reserveOrder, cmds.reserveLines)
	}
}

func TestHandleReleaseDecodesCommand(t *testing.T) {
	cmds := &fakeCommands{}
	data := mustMarshal(t, &inventoryv1.ReleaseStock{OrderId: "o-9"})

	err := NewHandlers(cmds).Handle(context.Background(), messaging.Message{ID: "m-1", Subject: subjects.InventoryCmdRelease, Data: data})
	if err != nil || cmds.releaseOrder != "o-9" {
		t.Fatalf("err=%v order=%q", err, cmds.releaseOrder)
	}
}

func TestHandleClassifiesErrors(t *testing.T) {
	validReserve := mustMarshal(t, &inventoryv1.ReserveStock{OrderId: "o-1", Items: []*commonv1.LineItem{{Sku: "A", Quantity: 1}}})
	transient := errors.New("connection reset")

	tests := []struct {
		name          string
		msg           messaging.Message
		cmdErr        error
		wantPermanent bool
		wantErr       bool
	}{
		{"garbage payload", messaging.Message{Subject: subjects.InventoryCmdReserve, Data: []byte{0xff, 0xff, 0xff}}, nil, true, true},
		{"unknown subject", messaging.Message{Subject: "inventory.cmd.nope"}, nil, true, true},
		{"invalid reservation is permanent", messaging.Message{Subject: subjects.InventoryCmdReserve, Data: validReserve}, domain.ErrInvalidReservation, true, true},
		{"transient failure is retryable", messaging.Message{Subject: subjects.InventoryCmdReserve, Data: validReserve}, transient, false, true},
		{"success", messaging.Message{Subject: subjects.InventoryCmdReserve, Data: validReserve}, nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHandlers(&fakeCommands{err: tc.cmdErr}).Handle(context.Background(), tc.msg)
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

func TestEventsEnqueueTypedMessagesWithCorrelation(t *testing.T) {
	ob := &fakeOutbox{}
	ev := NewEvents(ob)
	ctx := messaging.WithCorrelationID(context.Background(), "corr-1")

	if err := ev.StockReserved(ctx, "o-1"); err != nil {
		t.Fatal(err)
	}
	if err := ev.StockRejected(ctx, "o-2", "insufficient stock", []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	if err := ev.StockReleased(context.Background(), "o-3"); err != nil {
		t.Fatal(err)
	}

	if len(ob.msgs) != 3 {
		t.Fatalf("enqueued %d messages", len(ob.msgs))
	}

	var reserved inventoryv1.StockReserved
	if m := ob.msgs[0]; m.Subject != subjects.InventoryEvtReserved || proto.Unmarshal(m.Data, &reserved) != nil || reserved.GetOrderId() != "o-1" {
		t.Errorf("reserved message = %+v", m)
	}
	if ob.msgs[0].Headers[messaging.HeaderCorrelationID] != "corr-1" {
		t.Errorf("correlation ID not propagated: %v", ob.msgs[0].Headers)
	}
	if got := ob.msgs[0].Headers[messaging.HeaderMessageType]; got != "ecommerce.inventory.v1.StockReserved" {
		t.Errorf("message type = %q", got)
	}

	var rejected inventoryv1.StockRejected
	if m := ob.msgs[1]; m.Subject != subjects.InventoryEvtRejected || proto.Unmarshal(m.Data, &rejected) != nil ||
		rejected.GetReason() != "insufficient stock" || !reflect.DeepEqual(rejected.GetUnavailableSkus(), []string{"A", "B"}) {
		t.Errorf("rejected message = %+v / %v", ob.msgs[1], &rejected)
	}

	if m := ob.msgs[2]; m.Subject != subjects.InventoryEvtReleased {
		t.Errorf("released subject = %q", m.Subject)
	}
	if _, ok := ob.msgs[2].Headers[messaging.HeaderCorrelationID]; ok {
		t.Error("no correlation header expected when ctx has none")
	}
}
