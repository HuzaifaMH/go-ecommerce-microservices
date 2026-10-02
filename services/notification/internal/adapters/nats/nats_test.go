package nats

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

type fakeNotifier struct {
	confirmed []string
	cancelled []string
	total     domain.Money
	err       error
}

func (f *fakeNotifier) NotifyOrderConfirmed(_ context.Context, orderID, customerID string, total domain.Money) error {
	f.confirmed = append(f.confirmed, orderID+"/"+customerID)
	f.total = total
	return f.err
}

func (f *fakeNotifier) NotifyOrderCancelled(_ context.Context, orderID, customerID, reason string) error {
	f.cancelled = append(f.cancelled, orderID+"/"+customerID+"/"+reason)
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

func TestHandleRoutesBothEvents(t *testing.T) {
	n := &fakeNotifier{}
	h := NewHandlers(n)
	ctx := context.Background()

	confirmed := marshal(t, &orderv1.OrderConfirmed{OrderId: "o-1", CustomerId: "alice", Total: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 3000}})
	if err := h.Handle(ctx, messaging.Message{ID: "m-1", Subject: subjects.OrderEvtConfirmed, Data: confirmed}); err != nil {
		t.Fatal(err)
	}
	cancelled := marshal(t, &orderv1.OrderCancelled{OrderId: "o-2", CustomerId: "bob", Reason: "saga timeout"})
	if err := h.Handle(ctx, messaging.Message{ID: "m-2", Subject: subjects.OrderEvtCancelled, Data: cancelled}); err != nil {
		t.Fatal(err)
	}

	if len(n.confirmed) != 1 || n.confirmed[0] != "o-1/alice" || n.total != (domain.Money{CurrencyCode: "USD", AmountMinor: 3000}) {
		t.Errorf("confirmed = %v total = %+v", n.confirmed, n.total)
	}
	if len(n.cancelled) != 1 || n.cancelled[0] != "o-2/bob/saga timeout" {
		t.Errorf("cancelled = %v", n.cancelled)
	}
}

func TestHandleClassifiesErrors(t *testing.T) {
	valid := marshal(t, &orderv1.OrderCancelled{OrderId: "o-1", CustomerId: "alice", Reason: "x"})
	garbage := []byte{0xff, 0xff, 0xff}

	tests := []struct {
		name          string
		msg           messaging.Message
		notifyErr     error
		wantPermanent bool
		wantErr       bool
	}{
		{"garbage (confirmed)", messaging.Message{Subject: subjects.OrderEvtConfirmed, Data: garbage}, nil, true, true},
		{"garbage (cancelled)", messaging.Message{Subject: subjects.OrderEvtCancelled, Data: garbage}, nil, true, true},
		{"unexpected subject", messaging.Message{Subject: "payment.evt.failed"}, nil, true, true},
		{"event without data is permanent", messaging.Message{Subject: subjects.OrderEvtCancelled, Data: valid}, domain.ErrInvalidEvent, true, true},
		{"delivery problem is retried", messaging.Message{Subject: subjects.OrderEvtCancelled, Data: valid}, errors.New("sms gateway timeout"), false, true},
		{"success", messaging.Message{Subject: subjects.OrderEvtCancelled, Data: valid}, nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHandlers(&fakeNotifier{err: tc.notifyErr}).Handle(context.Background(), tc.msg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := errors.Is(err, messaging.ErrPermanent); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err %v)", got, tc.wantPermanent, err)
			}
		})
	}
}
