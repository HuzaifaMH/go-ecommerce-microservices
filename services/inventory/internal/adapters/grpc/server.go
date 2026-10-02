package grpc

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Inventory is the part of the application this adapter calls.
type Inventory interface {
	GetItem(ctx context.Context, sku string) (domain.Item, error)
	ListItems(ctx context.Context, pageSize int, afterSKU string) ([]domain.Item, string, error)
}

// Server implements inventoryv1.InventoryServiceServer.
type Server struct {
	inventoryv1.UnimplementedInventoryServiceServer
	inv Inventory
	log *slog.Logger
}

// NewServer returns the gRPC adapter.
func NewServer(inv Inventory, log *slog.Logger) *Server {
	return &Server{inv: inv, log: log}
}

func (s *Server) GetStock(ctx context.Context, req *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	if req.GetSku() == "" {
		return nil, status.Error(codes.InvalidArgument, "sku is required")
	}
	item, err := s.inv.GetItem(ctx, req.GetSku())
	if err != nil {
		return nil, s.toStatus("get stock", err)
	}
	return &inventoryv1.GetStockResponse{Item: toProto(item)}, nil
}

func (s *Server) ListStock(ctx context.Context, req *inventoryv1.ListStockRequest) (*inventoryv1.ListStockResponse, error) {
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

	items, next, err := s.inv.ListItems(ctx, pageSize, after)
	if err != nil {
		return nil, s.toStatus("list stock", err)
	}

	resp := &inventoryv1.ListStockResponse{Items: make([]*inventoryv1.StockItem, len(items))}
	for i, it := range items {
		resp.Items[i] = toProto(it)
	}
	if next != "" {
		resp.NextPageToken = encodeToken(next)
	}
	return resp, nil
}

// toStatus maps domain errors to gRPC codes. Unexpected errors are logged
// and reported as Internal without leaking details to the caller.
func (s *Server) toStatus(op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrItemNotFound):
		return status.Error(codes.NotFound, "item not found")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		s.log.Error("request failed", "op", op, "error", err)
		return status.Error(codes.Internal, "internal error")
	}
}

func toProto(it domain.Item) *inventoryv1.StockItem {
	return &inventoryv1.StockItem{
		Sku:  it.SKU,
		Name: it.Name,
		UnitPrice: &commonv1.Money{
			CurrencyCode: it.UnitPrice.CurrencyCode,
			AmountMinor:  it.UnitPrice.AmountMinor,
		},
		Available: clampInt32(it.Available()),
		Reserved:  clampInt32(it.Reserved),
	}
}

// clampInt32 saturates instead of wrapping around. Quantities come from INTEGER
// columns, so in practice they always fit.
func clampInt32(n int) int32 {
	return int32(max(math.MinInt32, min(math.MaxInt32, n))) //nolint:gosec // clamped to the int32 range
}

// Page tokens are opaque to clients; internally they are the last SKU of the previous page.
func encodeToken(sku string) string { return base64.RawURLEncoding.EncodeToString([]byte(sku)) }

func decodeToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	return string(b), err
}
