package grpc

import (
	"context"
	"errors"
	"fmt"
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

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/pagination"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

var created = time.Date(2026, 10, 2, 12, 0, 0, 123_000_000, time.UTC)

func sampleOrder(id string, status domain.Status) domain.Order {
	return domain.Order{
		ID: id, CustomerID: "c-1", Status: status, CancelReason: "",
		Items:     []domain.Item{{SKU: "A", Quantity: 2, UnitPrice: domain.Money{CurrencyCode: "USD", AmountMinor: 1000}}},
		Total:     domain.Money{CurrencyCode: "USD", AmountMinor: 2000},
		CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
}

// fakeOrders returns canned results and records what it was asked.
type fakeOrders struct {
	order     domain.Order
	orders    []domain.Order
	next      *app.Cursor
	err       error
	gotLines  []domain.Line
	gotKey    string
	gotAfter  *app.Cursor
	gotPage   int
	gotReason string
}

func (f *fakeOrders) CreateOrder(_ context.Context, _, key string, lines []domain.Line) (domain.Order, error) {
	f.gotKey, f.gotLines = key, lines
	return f.order, f.err
}
func (f *fakeOrders) GetOrder(context.Context, string) (domain.Order, error) { return f.order, f.err }
func (f *fakeOrders) ListOrders(_ context.Context, _ string, size int, after *app.Cursor) ([]domain.Order, *app.Cursor, error) {
	f.gotPage, f.gotAfter = size, after
	return f.orders, f.next, f.err
}
func (f *fakeOrders) CancelOrder(_ context.Context, _, reason string) (domain.Order, error) {
	f.gotReason = reason
	return f.order, f.err
}

func client(t *testing.T, o Orders) orderv1.OrderServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	orderv1.RegisterOrderServiceServer(srv, NewServer(o, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return orderv1.NewOrderServiceClient(conn)
}

func TestCreateOrderIgnoresClientPricesAndMapsTheOrder(t *testing.T) {
	f := &fakeOrders{order: sampleOrder("o-1", domain.StatusPending)}
	c := client(t, f)

	resp, err := c.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{
		CustomerId: "c-1", IdempotencyKey: "key-1",
		Items: []*orderv1.OrderItem{{Sku: "A", Quantity: 2, UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1}}}, // a client-supplied price
	})
	if err != nil {
		t.Fatal(err)
	}

	if f.gotKey != "key-1" || len(f.gotLines) != 1 || f.gotLines[0] != (domain.Line{SKU: "A", Quantity: 2}) {
		t.Errorf("application received key=%q lines=%v", f.gotKey, f.gotLines)
	}
	o := resp.GetOrder()
	if o.GetId() != "o-1" || o.GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PENDING || o.GetTotal().GetAmountMinor() != 2000 ||
		len(o.GetItems()) != 1 || o.GetItems()[0].GetUnitPrice().GetAmountMinor() != 1000 ||
		!o.GetCreateTime().AsTime().Equal(created) || !o.GetUpdateTime().AsTime().Equal(created.Add(time.Minute)) {
		t.Fatalf("order = %v", o)
	}
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[domain.Status]orderv1.OrderStatus{
		domain.StatusPending:       orderv1.OrderStatus_ORDER_STATUS_PENDING,
		domain.StatusStockReserved: orderv1.OrderStatus_ORDER_STATUS_STOCK_RESERVED,
		domain.StatusConfirmed:     orderv1.OrderStatus_ORDER_STATUS_CONFIRMED,
		domain.StatusCancelled:     orderv1.OrderStatus_ORDER_STATUS_CANCELLED,
		"something-else":           orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED,
	} {
		if got := toProtoStatus(status); got != want {
			t.Errorf("%s -> %v, want %v", status, got, want)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"invalid order", fmt.Errorf("%w: quantity must be positive", domain.ErrInvalidOrder), codes.InvalidArgument},
		{"unknown sku", &domain.UnknownSKUError{SKU: "X"}, codes.InvalidArgument},
		{"not found", domain.ErrOrderNotFound, codes.NotFound},
		{"idempotency conflict", domain.ErrIdempotencyConflict, codes.AlreadyExists},
		{"cannot cancel", domain.ErrCannotCancel, codes.FailedPrecondition},
		{"catalog down", fmt.Errorf("look up prices: %w", app.ErrCatalogUnavailable), codes.Unavailable},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"unexpected", errors.New("pq: connection refused"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, &fakeOrders{err: tc.err})
			_, err := c.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{CustomerId: "c", IdempotencyKey: "k"})
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

func TestInvalidOrderMessageHasNoGenericPrefix(t *testing.T) {
	c := client(t, &fakeOrders{err: fmt.Errorf("%w: quantity must be between 1 and 5", domain.ErrInvalidOrder)})
	_, err := c.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{})
	if got := status.Convert(err).Message(); got != "quantity must be between 1 and 5" {
		t.Fatalf("message = %q", got)
	}
}

func TestGetAndCancelRequireAnID(t *testing.T) {
	c := client(t, &fakeOrders{})
	if _, err := c.GetOrder(context.Background(), &orderv1.GetOrderRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetOrder: %v", err)
	}
	if _, err := c.CancelOrder(context.Background(), &orderv1.CancelOrderRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CancelOrder: %v", err)
	}
}

func TestCancelOrderPassesReasonAndReturnsTheOrder(t *testing.T) {
	f := &fakeOrders{order: sampleOrder("o-1", domain.StatusCancelled)}
	resp, err := client(t, f).CancelOrder(context.Background(), &orderv1.CancelOrderRequest{Id: "o-1", Reason: "changed my mind"})
	if err != nil || f.gotReason != "changed my mind" || resp.GetOrder().GetStatus() != orderv1.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("resp = %v, err = %v, reason = %q", resp, err, f.gotReason)
	}
}

func TestListOrdersPagination(t *testing.T) {
	next := &app.Cursor{CreatedAt: created, ID: "o-2"}
	f := &fakeOrders{orders: []domain.Order{sampleOrder("o-3", domain.StatusPending), sampleOrder("o-2", domain.StatusPending)}, next: next}
	c := client(t, f)

	resp, err := c.ListOrders(context.Background(), &orderv1.ListOrdersRequest{PageSize: 2})
	if err != nil || len(resp.GetOrders()) != 2 || resp.GetNextPageToken() == "" || f.gotPage != 2 || f.gotAfter != nil {
		t.Fatalf("resp = %v, err = %v, page = %d, after = %v", resp, err, f.gotPage, f.gotAfter)
	}

	// The token the server issued round-trips back into the same cursor.
	f.next = nil
	resp, err = c.ListOrders(context.Background(), &orderv1.ListOrdersRequest{PageSize: 2, PageToken: resp.GetNextPageToken()})
	if err != nil || resp.GetNextPageToken() != "" {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if f.gotAfter == nil || f.gotAfter.ID != "o-2" || !f.gotAfter.CreatedAt.Equal(created) {
		t.Fatalf("cursor = %+v, want the one from the first page", f.gotAfter)
	}
}

func TestListOrdersPageSizeAndTokenValidation(t *testing.T) {
	f := &fakeOrders{}
	c := client(t, f)
	ctx := context.Background()

	if _, err := c.ListOrders(ctx, &orderv1.ListOrdersRequest{PageSize: -1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative page size: %v", err)
	}
	for _, token := range []string{"!!!not-base64!!!", "bm9waXBl" /* "nopipe" */, "MTIzfA" /* "123|" (empty id) */} {
		if _, err := c.ListOrders(ctx, &orderv1.ListOrdersRequest{PageToken: token}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("token %q: %v", token, err)
		}
	}

	_, _ = c.ListOrders(ctx, &orderv1.ListOrdersRequest{})
	if f.gotPage != pagination.DefaultSize {
		t.Errorf("default page size = %d, want %d", f.gotPage, pagination.DefaultSize)
	}
	_, _ = c.ListOrders(ctx, &orderv1.ListOrdersRequest{PageSize: 100000})
	if f.gotPage != pagination.MaxSize {
		t.Errorf("page size was not capped: %d", f.gotPage)
	}
}
