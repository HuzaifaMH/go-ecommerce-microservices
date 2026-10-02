package grpc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Orders is the part of the application this adapter calls.
type Orders interface {
	CreateOrder(ctx context.Context, customerID, idempotencyKey string, lines []domain.Line) (domain.Order, error)
	GetOrder(ctx context.Context, id string) (domain.Order, error)
	ListOrders(ctx context.Context, customerID string, pageSize int, after *app.Cursor) ([]domain.Order, *app.Cursor, error)
	CancelOrder(ctx context.Context, id, reason string) (domain.Order, error)
}

// Server implements orderv1.OrderServiceServer.
type Server struct {
	orderv1.UnimplementedOrderServiceServer
	orders Orders
	log    *slog.Logger
}

// NewServer returns the gRPC adapter.
func NewServer(orders Orders, log *slog.Logger) *Server {
	return &Server{orders: orders, log: log}
}

// CreateOrder starts the order saga. Prices sent by the client are ignored:
// the order is priced from the inventory catalog.
func (s *Server) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	lines := make([]domain.Line, len(req.GetItems()))
	for i, it := range req.GetItems() {
		lines[i] = domain.Line{SKU: it.GetSku(), Quantity: int(it.GetQuantity())}
	}
	o, err := s.orders.CreateOrder(ctx, req.GetCustomerId(), req.GetIdempotencyKey(), lines)
	if err != nil {
		return nil, s.toStatus("create order", err)
	}
	return &orderv1.CreateOrderResponse{Order: toProto(o)}, nil
}

func (s *Server) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	o, err := s.orders.GetOrder(ctx, req.GetId())
	if err != nil {
		return nil, s.toStatus("get order", err)
	}
	return &orderv1.GetOrderResponse{Order: toProto(o)}, nil
}

func (s *Server) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	pageSize := int(req.GetPageSize())
	switch {
	case pageSize < 0:
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize > maxPageSize:
		pageSize = maxPageSize
	}
	after, err := decodeToken(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}

	orders, next, err := s.orders.ListOrders(ctx, req.GetCustomerId(), pageSize, after)
	if err != nil {
		return nil, s.toStatus("list orders", err)
	}
	resp := &orderv1.ListOrdersResponse{Orders: make([]*orderv1.Order, len(orders))}
	for i, o := range orders {
		resp.Orders[i] = toProto(o)
	}
	if next != nil {
		resp.NextPageToken = encodeToken(*next)
	}
	return resp, nil
}

// CancelOrder cancels an order that has not been confirmed yet.
func (s *Server) CancelOrder(ctx context.Context, req *orderv1.CancelOrderRequest) (*orderv1.CancelOrderResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	o, err := s.orders.CancelOrder(ctx, req.GetId(), req.GetReason())
	if err != nil {
		return nil, s.toStatus("cancel order", err)
	}
	return &orderv1.CancelOrderResponse{Order: toProto(o)}, nil
}

// toStatus maps domain errors to gRPC codes. Unexpected errors are logged and
// reported as Internal without leaking details to the caller.
func (s *Server) toStatus(op string, err error) error {
	var unknown *domain.UnknownSKUError
	switch {
	case errors.Is(err, domain.ErrInvalidOrder):
		return status.Error(codes.InvalidArgument, trimPrefix(err))
	case errors.As(err, &unknown):
		return status.Error(codes.InvalidArgument, unknown.Error())
	case errors.Is(err, domain.ErrOrderNotFound):
		return status.Error(codes.NotFound, "order not found")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, domain.ErrCannotCancel):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, app.ErrCatalogUnavailable):
		s.log.Error("catalog unavailable", "op", op, "error", err)
		return status.Error(codes.Unavailable, "pricing is temporarily unavailable, please retry")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		s.log.Error("request failed", "op", op, "error", err)
		return status.Error(codes.Internal, "internal error")
	}
}

// trimPrefix drops the generic "invalid order: " prefix so clients see just the reason.
func trimPrefix(err error) string {
	return strings.TrimPrefix(err.Error(), domain.ErrInvalidOrder.Error()+": ")
}

func toProto(o domain.Order) *orderv1.Order {
	items := make([]*orderv1.OrderItem, len(o.Items))
	for i, it := range o.Items {
		items[i] = &orderv1.OrderItem{
			Sku:      it.SKU,
			Quantity: clampInt32(it.Quantity),
			UnitPrice: &commonv1.Money{
				CurrencyCode: it.UnitPrice.CurrencyCode,
				AmountMinor:  it.UnitPrice.AmountMinor,
			},
		}
	}
	return &orderv1.Order{
		Id:           o.ID,
		CustomerId:   o.CustomerID,
		Items:        items,
		Total:        &commonv1.Money{CurrencyCode: o.Total.CurrencyCode, AmountMinor: o.Total.AmountMinor},
		Status:       toProtoStatus(o.Status),
		CancelReason: o.CancelReason,
		CreateTime:   timestamppb.New(o.CreatedAt),
		UpdateTime:   timestamppb.New(o.UpdatedAt),
	}
}

func toProtoStatus(s domain.Status) orderv1.OrderStatus {
	switch s {
	case domain.StatusPending:
		return orderv1.OrderStatus_ORDER_STATUS_PENDING
	case domain.StatusStockReserved:
		return orderv1.OrderStatus_ORDER_STATUS_STOCK_RESERVED
	case domain.StatusConfirmed:
		return orderv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case domain.StatusCancelled:
		return orderv1.OrderStatus_ORDER_STATUS_CANCELLED
	default:
		return orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

// clampInt32 saturates instead of wrapping around; quantities are bounded by
// domain.MaxQuantity, so in practice they always fit.
func clampInt32(n int) int32 {
	const maxInt32 = 1<<31 - 1
	return int32(min(n, maxInt32)) //nolint:gosec // clamped to the int32 range
}

// Page tokens are opaque to clients. Internally: base64url("<unix nanos>|<order id>").
func encodeToken(c app.Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeToken(token string) (*app.Cursor, error) {
	if token == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	nanos, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return nil, fmt.Errorf("malformed token")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, err
	}
	return &app.Cursor{CreatedAt: time.Unix(0, n).UTC(), ID: id}, nil
}
