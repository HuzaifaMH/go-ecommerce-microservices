package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

type fakePayments struct {
	payment domain.Payment
	err     error
}

func (f fakePayments) GetPayment(context.Context, string) (domain.Payment, error) {
	return f.payment, f.err
}

func client(t *testing.T, p Payments) paymentv1.PaymentServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	paymentv1.RegisterPaymentServiceServer(srv, NewServer(p, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return paymentv1.NewPaymentServiceClient(conn)
}

func TestGetPaymentMapsAllFields(t *testing.T) {
	created := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	c := client(t, fakePayments{payment: domain.Payment{
		ID: "p-1", OrderID: "o-1", Amount: domain.Money{CurrencyCode: "USD", AmountMinor: 1999},
		Status: domain.StatusFailed, FailureReason: "card declined", CreatedAt: created,
	}})

	resp, err := c.GetPayment(context.Background(), &paymentv1.GetPaymentRequest{OrderId: "o-1"})
	if err != nil {
		t.Fatal(err)
	}
	p := resp.GetPayment()
	if p.GetId() != "p-1" || p.GetOrderId() != "o-1" || p.GetAmount().GetAmountMinor() != 1999 || p.GetAmount().GetCurrencyCode() != "USD" ||
		p.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED || p.GetFailureReason() != "card declined" ||
		!p.GetCreateTime().AsTime().Equal(created) {
		t.Fatalf("unexpected payment: %v", p)
	}
}

func TestGetPaymentSucceededStatus(t *testing.T) {
	c := client(t, fakePayments{payment: domain.Payment{ID: "p-1", OrderID: "o-1", Status: domain.StatusSucceeded}})
	resp, err := c.GetPayment(context.Background(), &paymentv1.GetPaymentRequest{OrderId: "o-1"})
	if err != nil || resp.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_SUCCEEDED {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if resp.GetPayment().GetCreateTime() != nil {
		t.Error("a zero creation time must not be sent as 1970")
	}
}

func TestGetPaymentErrors(t *testing.T) {
	tests := []struct {
		name    string
		orderID string
		err     error
		want    codes.Code
	}{
		{"missing order id", "", nil, codes.InvalidArgument},
		{"not found", "o-1", domain.ErrPaymentNotFound, codes.NotFound},
		{"deadline", "o-1", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"unexpected error", "o-1", errors.New("pq: connection refused"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client(t, fakePayments{err: tc.err}).GetPayment(context.Background(), &paymentv1.GetPaymentRequest{OrderId: tc.orderID})
			st, _ := status.FromError(err)
			if st.Code() != tc.want {
				t.Fatalf("code = %v (%v), want %v", st.Code(), err, tc.want)
			}
			if tc.want == codes.Internal && st.Message() != "internal error" {
				t.Errorf("internal details leaked: %q", st.Message())
			}
		})
	}
}
