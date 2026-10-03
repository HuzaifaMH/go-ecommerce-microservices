package clients

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/requestid"
)

// recordingServer notes what the interceptors added to each call.
type recordingServer struct {
	inventoryv1.UnimplementedInventoryServiceServer
	mu         sync.Mutex
	requestIDs []string
	deadlines  []time.Duration // time left on the call's deadline, 0 if none
	block      bool
}

func (s *recordingServer) GetStock(ctx context.Context, _ *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	s.mu.Lock()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.requestIDs = append(s.requestIDs, md.Get(requestid.MetadataKey)...)
	}
	var left time.Duration
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	s.deadlines = append(s.deadlines, left)
	block := s.block
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &inventoryv1.GetStockResponse{}, nil
}

func (s *recordingServer) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requestIDs...)
}

func (s *recordingServer) firstDeadline() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadlines[0]
}

func dial(t *testing.T, srv *recordingServer, timeout time.Duration) inventoryv1.InventoryServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := Dial("passthrough:///bufnet", timeout,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return inventoryv1.NewInventoryServiceClient(conn)
}

func TestRequestIDIsForwardedToTheBackend(t *testing.T) {
	srv := &recordingServer{}
	c := dial(t, srv, 5*time.Second)

	ctx := requestid.With(context.Background(), "req-123")
	if _, err := c.GetStock(ctx, &inventoryv1.GetStockRequest{Sku: "A"}); err != nil {
		t.Fatal(err)
	}
	if ids := srv.ids(); len(ids) != 1 || ids[0] != "req-123" {
		t.Fatalf("backend saw request IDs %v, want [req-123]", ids)
	}
}

func TestNoRequestIDMeansNoMetadata(t *testing.T) {
	srv := &recordingServer{}
	c := dial(t, srv, 5*time.Second)

	if _, err := c.GetStock(context.Background(), &inventoryv1.GetStockRequest{Sku: "A"}); err != nil {
		t.Fatal(err)
	}
	if ids := srv.ids(); len(ids) != 0 {
		t.Fatalf("backend saw request IDs %v, want none", ids)
	}
}

func TestDefaultDeadlineIsAppliedWhenTheCallerSetNone(t *testing.T) {
	srv := &recordingServer{}
	c := dial(t, srv, 5*time.Second)

	if _, err := c.GetStock(context.Background(), &inventoryv1.GetStockRequest{Sku: "A"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.firstDeadline(); got <= 0 || got > 5*time.Second {
		t.Fatalf("deadline left = %v, want within (0, 5s]", got)
	}
}

func TestCallersDeadlineWins(t *testing.T) {
	srv := &recordingServer{}
	c := dial(t, srv, time.Hour) // a huge default must not override the caller's

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.GetStock(ctx, &inventoryv1.GetStockRequest{Sku: "A"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.firstDeadline(); got > 2*time.Second {
		t.Fatalf("deadline left = %v, want at most the caller's 2s", got)
	}
}

func TestSlowBackendIsCutOffByTheDefaultDeadline(t *testing.T) {
	srv := &recordingServer{block: true}
	c := dial(t, srv, 100*time.Millisecond)

	start := time.Now()
	_, err := c.GetStock(context.Background(), &inventoryv1.GetStockRequest{Sku: "A"})
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v; the default deadline was not enforced", time.Since(start))
	}
}
