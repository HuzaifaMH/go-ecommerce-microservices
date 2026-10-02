package grpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

// Payments is the part of the application this adapter calls.
type Payments interface {
	GetPayment(ctx context.Context, orderID string) (domain.Payment, error)
}

// Server implements paymentv1.PaymentServiceServer.
type Server struct {
	paymentv1.UnimplementedPaymentServiceServer
	payments Payments
	log      *slog.Logger
}

// NewServer returns the gRPC adapter.
func NewServer(payments Payments, log *slog.Logger) *Server {
	return &Server{payments: payments, log: log}
}

func (s *Server) GetPayment(ctx context.Context, req *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
	if req.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "order_id is required")
	}
	p, err := s.payments.GetPayment(ctx, req.GetOrderId())
	switch {
	case err == nil:
		return &paymentv1.GetPaymentResponse{Payment: toProto(p)}, nil
	case errors.Is(err, domain.ErrPaymentNotFound):
		return nil, status.Error(codes.NotFound, "payment not found")
	case errors.Is(err, context.Canceled):
		return nil, status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		// Log the details, but do not leak them to the caller.
		s.log.Error("get payment failed", "order_id", req.GetOrderId(), "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
}

func toProto(p domain.Payment) *paymentv1.Payment {
	out := &paymentv1.Payment{
		Id:      p.ID,
		OrderId: p.OrderID,
		Amount: &commonv1.Money{
			CurrencyCode: p.Amount.CurrencyCode,
			AmountMinor:  p.Amount.AmountMinor,
		},
		Status:        toProtoStatus(p.Status),
		FailureReason: p.FailureReason,
	}
	if !p.CreatedAt.IsZero() {
		out.CreateTime = timestamppb.New(p.CreatedAt)
	}
	return out
}

func toProtoStatus(s domain.Status) paymentv1.PaymentStatus {
	switch s {
	case domain.StatusSucceeded:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_SUCCEEDED
	case domain.StatusFailed:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED
	default:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED
	}
}
