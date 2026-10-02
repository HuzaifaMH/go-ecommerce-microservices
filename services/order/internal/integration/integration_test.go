// Package integration exercises the order service end to end against a real
// PostgreSQL (testcontainers) and an in-process NATS JetStream server.
//
// The inventory and payment services are stood in for by small scripted
// participants, so every saga path can be driven on demand, including a
// participant that never answers. The behaviour is chosen by the data:
//
//	SKU "OOS"          -> inventory answers StockRejected
//	SKU "SILENT"       -> inventory never answers
//	customer "decline-*" -> payment answers PaymentFailed
//	customer "silent-*"  -> payment never answers
//
// Tests are skipped with -short or when Docker is not available.
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	inventoryadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/inventory"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakePricing is the inventory gRPC API the order service prices from.
type fakePricing struct {
	inventoryv1.UnimplementedInventoryServiceServer
	prices map[string]int64
}

func (f fakePricing) GetStock(_ context.Context, req *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	p, ok := f.prices[req.GetSku()]
	if !ok {
		return nil, status.Error(codes.NotFound, "item not found")
	}
	return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{
		Sku: req.GetSku(), UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: p},
	}}, nil
}

// seen records everything the order service sent, as observed on the broker.
type seen struct {
	mu        sync.Mutex
	reserve   []string // order IDs
	release   []string // order IDs, one entry per command
	charge    []*paymentv1.ChargePayment
	confirmed map[string]int
	cancelled map[string]string // order ID -> reason
	corr      map[string]string // order ID -> correlation ID on its reserve command
}

func (s *seen) count(list *[]string, orderID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range *list {
		if id == orderID {
			n++
		}
	}
	return n
}

func (s *seen) releases(orderID string) int { return s.count(&s.release, orderID) }
func (s *seen) reserves(orderID string) int { return s.count(&s.reserve, orderID) }

func (s *seen) charges(orderID string) []*paymentv1.ChargePayment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*paymentv1.ChargePayment
	for _, c := range s.charge {
		if c.GetOrderId() == orderID {
			out = append(out, c)
		}
	}
	return out
}

type env struct {
	pool *pgxpool.Pool
	svc  *app.Service
	js   jetstream.JetStream
	pub  *messaging.JetStreamPublisher
	got  *seen
}

// newEnv starts the whole stack. sagaTimeout is how long an order may wait for a reply.
func newEnv(t *testing.T, sagaTimeout time.Duration) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// PostgreSQL
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("orders"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pgadapter.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// NATS JetStream with the three streams of the saga
	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, js, err := messaging.Connect(ns.ClientURL(), "order-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	streams := map[string]jetstream.Stream{}
	for _, cfg := range []messaging.StreamConfig{subjects.OrderStream, subjects.InventoryStream, subjects.PaymentStream} {
		s, err := messaging.EnsureStream(ctx, js, cfg)
		if err != nil {
			t.Fatal(err)
		}
		streams[cfg.Name] = s
	}
	pub := messaging.NewJetStreamPublisher(js)

	// Pricing over a real gRPC connection (in memory).
	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(grpcSrv, fakePricing{prices: map[string]int64{"A": 1000, "B": 250, "OOS": 100, "SILENT": 100}})
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The order service, wired like cmd/order/main.go.
	store := pgstore.New(pool)
	repo := pgadapter.NewRepository(store)
	publisher := natsadapter.NewPublisher(store)
	svc := app.NewService(app.Deps{
		Catalog: inventoryadapter.NewClient(conn, 2*time.Second), Repo: repo, Tx: repo, Commands: publisher, Events: publisher,
		Log: discard, NewID: uuid.NewString, Now: func() time.Time { return time.Now().UTC() },
	})
	go func() {
		_ = outbox.NewRelay(store, pub, discard, outbox.Options{PollInterval: 20 * time.Millisecond}).Run(ctx)
	}()
	go func() { _ = natsadapter.Consume(ctx, js, store, natsadapter.NewHandlers(svc), discard) }()
	go func() { _ = svc.RunSweeper(ctx, sagaTimeout, 100*time.Millisecond, 100) }()

	got := &seen{confirmed: map[string]int{}, cancelled: map[string]string{}, corr: map[string]string{}}
	e := &env{pool: pool, svc: svc, js: js, pub: pub, got: got}

	// Scripted participants and an observer of order events.
	e.startParticipants(ctx, t, streams)
	return e
}

func (e *env) startParticipants(ctx context.Context, t *testing.T, streams map[string]jetstream.Stream) {
	t.Helper()

	consume := func(stream, durable string, filters []string, h func(jetstream.Msg)) {
		cons, err := streams[stream].CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable: durable, FilterSubjects: filters, AckPolicy: jetstream.AckExplicitPolicy,
		})
		if err != nil {
			t.Fatal(err)
		}
		cc, err := cons.Consume(func(m jetstream.Msg) { h(m); _ = m.Ack() })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cc.Stop)
	}
	reply := func(subject string, msg proto.Message, corr string) {
		data, _ := proto.Marshal(msg)
		m := messaging.Message{ID: uuid.NewString(), Subject: subject, Data: data}
		if corr != "" {
			m.Headers = map[string]string{messaging.HeaderCorrelationID: corr}
		}
		_ = e.pub.Publish(ctx, m)
	}

	// Inventory stand-in.
	consume("INVENTORY", "test-inventory", []string{subjects.InventoryCmdReserve, subjects.InventoryCmdRelease}, func(m jetstream.Msg) {
		switch m.Subject() {
		case subjects.InventoryCmdReserve:
			var cmd inventoryv1.ReserveStock
			_ = proto.Unmarshal(m.Data(), &cmd)
			e.got.mu.Lock()
			e.got.reserve = append(e.got.reserve, cmd.GetOrderId())
			e.got.corr[cmd.GetOrderId()] = m.Headers().Get(messaging.HeaderCorrelationID)
			e.got.mu.Unlock()
			for _, it := range cmd.GetItems() {
				switch it.GetSku() {
				case "SILENT":
					return
				case "OOS":
					reply(subjects.InventoryEvtRejected, &inventoryv1.StockRejected{OrderId: cmd.GetOrderId(), Reason: "insufficient stock", UnavailableSkus: []string{"OOS"}}, "")
					return
				}
			}
			reply(subjects.InventoryEvtReserved, &inventoryv1.StockReserved{OrderId: cmd.GetOrderId()}, "")
		case subjects.InventoryCmdRelease:
			var cmd inventoryv1.ReleaseStock
			_ = proto.Unmarshal(m.Data(), &cmd)
			e.got.mu.Lock()
			e.got.release = append(e.got.release, cmd.GetOrderId())
			e.got.mu.Unlock()
			reply(subjects.InventoryEvtReleased, &inventoryv1.StockReleased{OrderId: cmd.GetOrderId()}, "")
		}
	})

	// Payment stand-in.
	consume("PAYMENT", "test-payment", []string{subjects.PaymentCmdCharge}, func(m jetstream.Msg) {
		var cmd paymentv1.ChargePayment
		_ = proto.Unmarshal(m.Data(), &cmd)
		e.got.mu.Lock()
		e.got.charge = append(e.got.charge, &cmd)
		e.got.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd.GetCustomerId(), "silent-"):
		case strings.HasPrefix(cmd.GetCustomerId(), "decline-"):
			reply(subjects.PaymentEvtFailed, &paymentv1.PaymentFailed{OrderId: cmd.GetOrderId(), Reason: "card declined"}, "")
		default:
			reply(subjects.PaymentEvtSucceeded, &paymentv1.PaymentSucceeded{OrderId: cmd.GetOrderId(), PaymentId: "pay-" + cmd.GetOrderId()}, "")
		}
	})

	// Observer of the events other services (notification) would consume.
	consume("ORDER", "test-observer", []string{subjects.OrderEvtConfirmed, subjects.OrderEvtCancelled}, func(m jetstream.Msg) {
		e.got.mu.Lock()
		defer e.got.mu.Unlock()
		switch m.Subject() {
		case subjects.OrderEvtConfirmed:
			var ev orderv1.OrderConfirmed
			_ = proto.Unmarshal(m.Data(), &ev)
			e.got.confirmed[ev.GetOrderId()]++
		case subjects.OrderEvtCancelled:
			var ev orderv1.OrderCancelled
			_ = proto.Unmarshal(m.Data(), &ev)
			e.got.cancelled[ev.GetOrderId()] = ev.GetReason()
		}
	})
}

var ctx = context.Background()

func asUnknownSKU(err error, target **domain.UnknownSKUError) bool { return errors.As(err, target) }

func (e *env) create(t *testing.T, customer, key string, lines ...domain.Line) domain.Order {
	t.Helper()
	o, err := e.svc.CreateOrder(ctx, customer, key, lines)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) order(t *testing.T, id string) domain.Order {
	t.Helper()
	o, err := e.svc.GetOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) waitStatus(t *testing.T, id string, want domain.Status) domain.Order {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if o := e.order(t, id); o.Status == want {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("order %s is %s, want %s", id, e.order(t, id).Status, want)
	return domain.Order{}
}

// replyLater injects a reply as if a slow participant answered after the fact.
func (e *env) replyLater(t *testing.T, subject string, msg proto.Message) {
	t.Helper()
	data, _ := proto.Marshal(msg)
	if err := e.pub.Publish(ctx, messaging.Message{ID: uuid.NewString(), Subject: subject, Data: data}); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) confirmedEvents(id string) int {
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	return e.got.confirmed[id]
}

func (e *env) cancelReason(id string) (string, bool) {
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	r, ok := e.got.cancelled[id]
	return r, ok
}

const long = 5 * time.Minute // "no timeout" for tests that are not about the timeout

func TestMigrateIsIdempotent(t *testing.T) {
	e := newEnv(t, long)
	if err := pgadapter.Migrate(ctx, e.pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestHappyPathConfirmsTheOrderAndAnnouncesIt(t *testing.T) {
	e := newEnv(t, long)

	o := e.create(t, "alice", "key-1", domain.Line{SKU: "A", Quantity: 2}, domain.Line{SKU: "B", Quantity: 4})
	if o.Status != domain.StatusPending || o.Total.AmountMinor != 3000 {
		t.Fatalf("created order = %+v", o)
	}

	done := e.waitStatus(t, o.ID, domain.StatusConfirmed)
	eventually(t, "OrderConfirmed event", func() bool { return e.confirmedEvents(o.ID) == 1 })

	if len(done.Items) != 2 || done.Items[0].SKU != "A" || done.Items[0].UnitPrice.AmountMinor != 1000 {
		t.Errorf("items = %+v", done.Items)
	}
	charges := e.got.charges(o.ID)
	if len(charges) != 1 || charges[0].GetCustomerId() != "alice" || charges[0].GetAmount().GetAmountMinor() != 3000 {
		t.Errorf("charge commands = %v", charges)
	}
	if e.got.releases(o.ID) != 0 {
		t.Error("a successful order must not release stock")
	}
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	if e.got.corr[o.ID] != o.ID {
		t.Errorf("correlation ID on the first command = %q, want the order ID", e.got.corr[o.ID])
	}
}

func TestOutOfStockCancelsWithoutReleasing(t *testing.T) {
	e := newEnv(t, long)

	o := e.create(t, "alice", "key-1", domain.Line{SKU: "OOS", Quantity: 1})
	done := e.waitStatus(t, o.ID, domain.StatusCancelled)

	if !strings.Contains(done.CancelReason, "out of stock") {
		t.Errorf("reason = %q", done.CancelReason)
	}
	eventually(t, "OrderCancelled event", func() bool { _, ok := e.cancelReason(o.ID); return ok })
	if n := len(e.got.charges(o.ID)); n != 0 {
		t.Errorf("an order without stock must never be charged, got %d charges", n)
	}
	if e.got.releases(o.ID) != 0 {
		t.Error("nothing was reserved, so nothing may be released")
	}
}

func TestDeclinedPaymentRollsBackByReleasingStock(t *testing.T) {
	e := newEnv(t, long)

	o := e.create(t, "decline-bob", "key-1", domain.Line{SKU: "A", Quantity: 1})
	done := e.waitStatus(t, o.ID, domain.StatusCancelled)

	if done.CancelReason != "payment failed: card declined" {
		t.Errorf("reason = %q", done.CancelReason)
	}
	eventually(t, "stock released", func() bool { return e.got.releases(o.ID) == 1 })
	eventually(t, "OrderCancelled event", func() bool { r, ok := e.cancelReason(o.ID); return ok && r == done.CancelReason })
}

func TestDuplicateAndLateRepliesChangeNothing(t *testing.T) {
	e := newEnv(t, long)
	o := e.create(t, "alice", "key-1", domain.Line{SKU: "A", Quantity: 1})
	e.waitStatus(t, o.ID, domain.StatusConfirmed)
	eventually(t, "OrderConfirmed event", func() bool { return e.confirmedEvents(o.ID) == 1 })

	// The same replies again under new message IDs, plus contradictory late ones.
	for range 2 {
		e.replyLater(t, subjects.InventoryEvtReserved, &inventoryv1.StockReserved{OrderId: o.ID})
		e.replyLater(t, subjects.PaymentEvtSucceeded, &paymentv1.PaymentSucceeded{OrderId: o.ID, PaymentId: "again"})
		e.replyLater(t, subjects.PaymentEvtFailed, &paymentv1.PaymentFailed{OrderId: o.ID, Reason: "late"})
		e.replyLater(t, subjects.InventoryEvtRejected, &inventoryv1.StockRejected{OrderId: o.ID, Reason: "late"})
	}
	// A marker order proves the duplicates (published earlier on the same streams) were processed.
	marker := e.create(t, "alice", "key-2", domain.Line{SKU: "A", Quantity: 1})
	e.waitStatus(t, marker.ID, domain.StatusConfirmed)
	time.Sleep(300 * time.Millisecond)

	if got := e.order(t, o.ID).Status; got != domain.StatusConfirmed {
		t.Fatalf("status = %s, want it to stay confirmed", got)
	}
	if n := e.confirmedEvents(o.ID); n != 1 {
		t.Errorf("OrderConfirmed published %d times, want 1", n)
	}
	if n := len(e.got.charges(o.ID)); n != 1 {
		t.Errorf("customer charged %d times, want 1", n)
	}
}

func TestConcurrentIdenticalRequestsCreateOneOrder(t *testing.T) {
	e := newEnv(t, long)
	const attempts = 10

	ids := make(chan string, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := e.svc.CreateOrder(ctx, "alice", "same-key", []domain.Line{{SKU: "A", Quantity: 1}})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- o.ID
		}()
	}
	wg.Wait()
	close(ids)

	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("different orders for one idempotency key: %s and %s", first, id)
		}
	}
	e.waitStatus(t, first, domain.StatusConfirmed)
	if n := e.got.reserves(first); n != 1 {
		t.Fatalf("stock reservation requested %d times, want 1", n)
	}
	var rows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("orders = %d, err = %v; want 1", rows, err)
	}
}

func TestReusedKeyWithDifferentItemsIsRejected(t *testing.T) {
	e := newEnv(t, long)
	e.create(t, "alice", "key-1", domain.Line{SKU: "A", Quantity: 1})

	_, err := e.svc.CreateOrder(ctx, "alice", "key-1", []domain.Line{{SKU: "A", Quantity: 2}})
	if err == nil || err.Error() != domain.ErrIdempotencyConflict.Error() {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestUnknownSKUIsRejectedBeforeAnythingIsStored(t *testing.T) {
	e := newEnv(t, long)

	_, err := e.svc.CreateOrder(ctx, "alice", "key-1", []domain.Line{{SKU: "NOPE", Quantity: 1}})
	var unknown *domain.UnknownSKUError
	if !asUnknownSKU(err, &unknown) || unknown.SKU != "NOPE" {
		t.Fatalf("err = %v, want UnknownSKUError(NOPE)", err)
	}
	var rows int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d orders stored for a rejected request", rows)
	}
}

func TestCustomerCancellationReleasesStockAndLateReservationIsCompensated(t *testing.T) {
	e := newEnv(t, long)

	// SILENT: inventory never answers, so the order stays pending and cancellable.
	o := e.create(t, "alice", "key-1", domain.Line{SKU: "SILENT", Quantity: 1})
	cancelled, err := e.svc.CancelOrder(ctx, o.ID, "changed my mind")
	if err != nil || cancelled.Status != domain.StatusCancelled || cancelled.CancelReason != "changed my mind" {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	eventually(t, "release command", func() bool { return e.got.releases(o.ID) == 1 })
	eventually(t, "OrderCancelled event", func() bool { r, ok := e.cancelReason(o.ID); return ok && r == "changed my mind" })

	// The reservation that was in flight arrives after the cancellation.
	e.replyLater(t, subjects.InventoryEvtReserved, &inventoryv1.StockReserved{OrderId: o.ID})
	eventually(t, "second release for the late reservation", func() bool { return e.got.releases(o.ID) == 2 })
	if got := e.order(t, o.ID).Status; got != domain.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", got)
	}
	if n := len(e.got.charges(o.ID)); n != 0 {
		t.Errorf("a cancelled order must not be charged, got %d", n)
	}
}

func TestConfirmedOrderCannotBeCancelled(t *testing.T) {
	e := newEnv(t, long)
	o := e.create(t, "alice", "key-1", domain.Line{SKU: "A", Quantity: 1})
	e.waitStatus(t, o.ID, domain.StatusConfirmed)

	_, err := e.svc.CancelOrder(ctx, o.ID, "too late")
	if err == nil || err.Error() != domain.ErrCannotCancel.Error() {
		t.Fatalf("err = %v, want ErrCannotCancel", err)
	}
	if got := e.order(t, o.ID).Status; got != domain.StatusConfirmed {
		t.Fatalf("status = %s", got)
	}
}

func TestSilentInventoryTimesOutAndOrderIsCancelled(t *testing.T) {
	e := newEnv(t, 700*time.Millisecond)

	o := e.create(t, "alice", "key-1", domain.Line{SKU: "SILENT", Quantity: 1})
	done := e.waitStatus(t, o.ID, domain.StatusCancelled)

	if done.CancelReason != "saga timeout" {
		t.Errorf("reason = %q", done.CancelReason)
	}
	eventually(t, "stock released after the timeout", func() bool { return e.got.releases(o.ID) == 1 })
	eventually(t, "OrderCancelled event", func() bool { r, ok := e.cancelReason(o.ID); return ok && r == "saga timeout" })
}

func TestSilentPaymentTimesOutAndLateSuccessIsFlaggedForRefund(t *testing.T) {
	e := newEnv(t, 700*time.Millisecond)

	// Stock is reserved, then payment never answers.
	o := e.create(t, "silent-carol", "key-1", domain.Line{SKU: "A", Quantity: 1})
	done := e.waitStatus(t, o.ID, domain.StatusCancelled)
	if done.CancelReason != "saga timeout" {
		t.Errorf("reason = %q", done.CancelReason)
	}
	eventually(t, "reserved stock released", func() bool { return e.got.releases(o.ID) == 1 })

	// The payment finally goes through: the customer paid for a cancelled order.
	e.replyLater(t, subjects.PaymentEvtSucceeded, &paymentv1.PaymentSucceeded{OrderId: o.ID, PaymentId: "late"})
	eventually(t, "refund flag", func() bool {
		var flagged bool
		_ = e.pool.QueryRow(ctx, `SELECT refund_required FROM orders WHERE id = $1`, o.ID).Scan(&flagged)
		return flagged
	})
	if got := e.order(t, o.ID).Status; got != domain.StatusCancelled {
		t.Fatalf("status = %s, a late payment must not resurrect the order", got)
	}
}

func TestManyConcurrentOrdersAllReachAFinalState(t *testing.T) {
	e := newEnv(t, long)
	const orders = 24

	type result struct {
		id   string
		want domain.Status
	}
	results := make(chan result, orders)
	var wg sync.WaitGroup
	for i := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			customer, sku, want := "alice", "A", domain.StatusConfirmed
			switch i % 3 {
			case 1:
				customer, want = "decline-bob", domain.StatusCancelled
			case 2:
				sku, want = "OOS", domain.StatusCancelled
			}
			o, err := e.svc.CreateOrder(ctx, customer, fmt.Sprintf("key-%02d", i), []domain.Line{{SKU: sku, Quantity: 1}})
			if err != nil {
				t.Error(err)
				return
			}
			results <- result{o.ID, want}
		}()
	}
	wg.Wait()
	close(results)

	for r := range results {
		e.waitStatus(t, r.id, r.want)
	}
	var confirmed, cancelled int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'confirmed'), count(*) FILTER (WHERE status = 'cancelled') FROM orders`).
		Scan(&confirmed, &cancelled); err != nil {
		t.Fatal(err)
	}
	if confirmed != orders/3 || cancelled != orders-orders/3 {
		t.Fatalf("confirmed=%d cancelled=%d, want %d and %d", confirmed, cancelled, orders/3, orders-orders/3)
	}
}

func TestListOrdersPaginatesNewestFirstPerCustomer(t *testing.T) {
	e := newEnv(t, long)

	var mine []string
	for i := range 5 {
		o := e.create(t, "alice", fmt.Sprintf("k-%d", i), domain.Line{SKU: "SILENT", Quantity: 1})
		mine = append([]string{o.ID}, mine...) // newest first
		time.Sleep(5 * time.Millisecond)
	}
	e.create(t, "bob", "k-bob", domain.Line{SKU: "SILENT", Quantity: 1})

	var got []string
	var after *app.Cursor
	for range 10 {
		page, next, err := e.svc.ListOrders(ctx, "alice", 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page {
			if len(o.Items) != 1 || o.Items[0].SKU != "SILENT" {
				t.Fatalf("order %s was listed without its items: %+v", o.ID, o.Items)
			}
			got = append(got, o.ID)
		}
		if next == nil {
			break
		}
		after = next
	}
	if fmt.Sprint(got) != fmt.Sprint(mine) {
		t.Fatalf("paged = %v\nwant   = %v", got, mine)
	}

	all, _, err := e.svc.ListOrders(ctx, "", 50, nil)
	if err != nil || len(all) != 6 {
		t.Fatalf("all customers: %d orders, err %v; want 6", len(all), err)
	}
}

func TestGetOrderUnknownID(t *testing.T) {
	e := newEnv(t, long)
	if _, err := e.svc.GetOrder(ctx, "nope"); err == nil || err.Error() != domain.ErrOrderNotFound.Error() {
		t.Fatalf("err = %v, want ErrOrderNotFound", err)
	}
}
