package inventory

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

// fakeInventory serves GetStock from a price list; a SKU can be made slow or broken.
type fakeInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
	prices map[string]int64
	slow   string
	broken string
	calls  atomic.Int32
}

func (f *fakeInventory) GetStock(ctx context.Context, req *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	f.calls.Add(1)
	if req.GetSku() == f.slow {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if req.GetSku() == f.broken {
		return nil, status.Error(codes.Internal, "boom")
	}
	p, ok := f.prices[req.GetSku()]
	if !ok {
		return nil, status.Error(codes.NotFound, "item not found")
	}
	return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{
		Sku: req.GetSku(), UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: p},
	}}, nil
}

func newClient(t *testing.T, f *fakeInventory, timeout time.Duration) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewClient(conn, timeout)
}

func TestPricesFetchesEverySKU(t *testing.T) {
	f := &fakeInventory{prices: map[string]int64{"A": 1000, "B": 250, "C": 5}}
	c := newClient(t, f, time.Second)

	got, err := c.Prices(context.Background(), []string{"A", "B", "C"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]domain.Money{
		"A": {CurrencyCode: "USD", AmountMinor: 1000},
		"B": {CurrencyCode: "USD", AmountMinor: 250},
		"C": {CurrencyCode: "USD", AmountMinor: 5},
	}
	if len(got) != 3 || got["A"] != want["A"] || got["B"] != want["B"] || got["C"] != want["C"] {
		t.Fatalf("prices = %v", got)
	}
}

func TestUnknownSKUIsReportedAsSuch(t *testing.T) {
	c := newClient(t, &fakeInventory{prices: map[string]int64{"A": 1}}, time.Second)

	_, err := c.Prices(context.Background(), []string{"A", "NOPE"})
	var unknown *domain.UnknownSKUError
	if !errors.As(err, &unknown) || unknown.SKU != "NOPE" {
		t.Fatalf("err = %v, want UnknownSKUError(NOPE)", err)
	}
	if errors.Is(err, app.ErrCatalogUnavailable) {
		t.Error("an unknown SKU is not an outage")
	}
}

func TestInventoryFailuresAreReportedAsUnavailable(t *testing.T) {
	c := newClient(t, &fakeInventory{prices: map[string]int64{"A": 1}, broken: "BAD"}, time.Second)

	_, err := c.Prices(context.Background(), []string{"A", "BAD"})
	if !errors.Is(err, app.ErrCatalogUnavailable) {
		t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
	}
}

func TestSlowInventoryTimesOutPerCall(t *testing.T) {
	c := newClient(t, &fakeInventory{prices: map[string]int64{"A": 1}, slow: "SLOW"}, 50*time.Millisecond)

	start := time.Now()
	_, err := c.Prices(context.Background(), []string{"A", "SLOW"})
	if !errors.Is(err, app.ErrCatalogUnavailable) {
		t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v; the per-call timeout was not applied", time.Since(start))
	}
}

func TestCancelledContextStopsTheLookup(t *testing.T) {
	c := newClient(t, &fakeInventory{prices: map[string]int64{"A": 1}}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Prices(ctx, []string{"A"}); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
