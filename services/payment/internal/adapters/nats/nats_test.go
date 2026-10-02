package nats

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

type fakeCommands struct {
	order, customer string
	amount          domain.Money
	err             error
}

func (f *fakeCommands) Charge(_ context.Context, orderID, customerID string, amount domain.Money) error {
	f.order, f.customer, f.amount = orderID, customerID, amount
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

func TestHandleDecodesChargeCommand(t *testing.T) {
	cmds := &fakeCommands{}
	data := marshal(t, &paymentv1.ChargePayment{
		OrderId: "o-1", CustomerId: "c-1", Amount: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1999},
	})

	err := NewHandlers(cmds).Handle(context.Background(), messaging.Message{ID: "m-1", Subject: subjects.PaymentCmdCharge, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if cmds.order != "o-1" || cmds.customer != "c-1" || cmds.amount != (domain.Money{CurrencyCode: "USD", AmountMinor: 1999}) {
		t.Fatalf("got %+v", cmds)
	}
}

func TestHandleClassifiesErrors(t *testing.T) {
	valid := marshal(t, &paymentv1.ChargePayment{OrderId: "o-1", CustomerId: "c-1", Amount: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1}})

	tests := []struct {
		name          string
		msg           messaging.Message
		cmdErr        error
		wantPermanent bool
		wantErr       bool
	}{
		{"garbage payload", messaging.Message{Subject: subjects.PaymentCmdCharge, Data: []byte{0xff, 0xff}}, nil, true, true},
		{"unexpected subject", messaging.Message{Subject: "payment.cmd.refund"}, nil, true, true},
		{"invalid payment is permanent", messaging.Message{Subject: subjects.PaymentCmdCharge, Data: valid}, domain.ErrInvalidPayment, true, true},
		{"provider outage is retried", messaging.Message{Subject: subjects.PaymentCmdCharge, Data: valid}, errors.New("provider timeout"), false, true},
		{"success", messaging.Message{Subject: subjects.PaymentCmdCharge, Data: valid}, nil, false, false},
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

	if err := ev.PaymentSucceeded(ctx, "o-1", "p-1"); err != nil {
		t.Fatal(err)
	}
	if err := ev.PaymentFailed(ctx, "o-2", "card declined"); err != nil {
		t.Fatal(err)
	}
	if len(ob.msgs) != 2 {
		t.Fatalf("enqueued %d messages", len(ob.msgs))
	}

	var ok paymentv1.PaymentSucceeded
	m := ob.msgs[0]
	if m.Subject != subjects.PaymentEvtSucceeded || proto.Unmarshal(m.Data, &ok) != nil || ok.GetOrderId() != "o-1" || ok.GetPaymentId() != "p-1" {
		t.Errorf("succeeded message = %+v / %v", m, &ok)
	}
	if m.Headers[messaging.HeaderCorrelationID] != "corr-1" || m.Headers[messaging.HeaderMessageType] != "ecommerce.payment.v1.PaymentSucceeded" {
		t.Errorf("headers = %v", m.Headers)
	}

	var failed paymentv1.PaymentFailed
	m = ob.msgs[1]
	if m.Subject != subjects.PaymentEvtFailed || proto.Unmarshal(m.Data, &failed) != nil || failed.GetOrderId() != "o-2" || failed.GetReason() != "card declined" {
		t.Errorf("failed message = %+v / %v", m, &failed)
	}
}
