package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

type fakeInventory struct {
	items map[string]domain.Item
	err   error
}

func (f *fakeInventory) GetItem(_ context.Context, sku string) (domain.Item, error) {
	if f.err != nil {
		return domain.Item{}, f.err
	}
	it, ok := f.items[sku]
	if !ok {
		return domain.Item{}, domain.ErrItemNotFound
	}
	return it, nil
}

func (f *fakeInventory) ListItems(_ context.Context, pageSize int, after string) ([]domain.Item, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	var skus []string
	for s := range f.items {
		if s > after {
			skus = append(skus, s)
		}
	}
	sort.Strings(skus)
	next := ""
	if len(skus) > pageSize {
		skus = skus[:pageSize]
		next = skus[len(skus)-1]
	}
	out := make([]domain.Item, len(skus))
	for i, s := range skus {
		out[i] = f.items[s]
	}
	return out, next, nil
}

// client starts the server on an in-memory listener and returns a connected client.
func client(t *testing.T, inv Inventory) inventoryv1.InventoryServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(srv, NewServer(inv, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return inventoryv1.NewInventoryServiceClient(conn)
}

func fixture() *fakeInventory {
	return &fakeInventory{items: map[string]domain.Item{
		"A": {SKU: "A", Name: "Alpha", UnitPrice: domain.Money{CurrencyCode: "USD", AmountMinor: 1999}, OnHand: 10, Reserved: 3},
		"B": {SKU: "B", OnHand: 1},
		"C": {SKU: "C", OnHand: 1},
	}}
}

func TestGetStock(t *testing.T) {
	c := client(t, fixture())

	resp, err := c.GetStock(context.Background(), &inventoryv1.GetStockRequest{Sku: "A"})
	if err != nil {
		t.Fatal(err)
	}
	it := resp.GetItem()
	if it.GetSku() != "A" || it.GetName() != "Alpha" || it.GetAvailable() != 7 || it.GetReserved() != 3 ||
		it.GetUnitPrice().GetAmountMinor() != 1999 || it.GetUnitPrice().GetCurrencyCode() != "USD" {
		t.Fatalf("unexpected item: %v", it)
	}
}

func TestGetStockErrors(t *testing.T) {
	tests := []struct {
		name string
		inv  *fakeInventory
		sku  string
		want codes.Code
	}{
		{"missing sku", fixture(), "", codes.InvalidArgument},
		{"unknown sku", fixture(), "nope", codes.NotFound},
		{"unexpected error hides details", &fakeInventory{err: errors.New("pq: password authentication failed")}, "A", codes.Internal},
		{"deadline", &fakeInventory{err: context.DeadlineExceeded}, "A", codes.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client(t, tc.inv).GetStock(context.Background(), &inventoryv1.GetStockRequest{Sku: tc.sku})
			st, _ := status.FromError(err)
			if st.Code() != tc.want {
				t.Fatalf("code = %v (%v), want %v", st.Code(), err, tc.want)
			}
			if tc.want == codes.Internal && st.Message() != "internal error" {
				t.Errorf("internal details leaked to client: %q", st.Message())
			}
		})
	}
}

func TestListStockPagination(t *testing.T) {
	c := client(t, fixture())
	ctx := context.Background()

	p1, err := c.ListStock(ctx, &inventoryv1.ListStockRequest{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.GetItems()) != 2 || p1.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %v", p1)
	}

	p2, err := c.ListStock(ctx, &inventoryv1.ListStockRequest{PageSize: 2, PageToken: p1.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.GetItems()) != 1 || p2.GetItems()[0].GetSku() != "C" || p2.GetNextPageToken() != "" {
		t.Fatalf("page 2 = %v", p2)
	}
}

func TestListStockValidation(t *testing.T) {
	c := client(t, fixture())
	ctx := context.Background()

	for name, req := range map[string]*inventoryv1.ListStockRequest{
		"negative page size": {PageSize: -1},
		"garbage token":      {PageToken: "!!!not-base64!!!"},
	} {
		_, err := c.ListStock(ctx, req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}

	resp, err := c.ListStock(ctx, &inventoryv1.ListStockRequest{}) // default page size
	if err != nil || len(resp.GetItems()) != 3 {
		t.Fatalf("default page size: %v %v", resp, err)
	}
}
