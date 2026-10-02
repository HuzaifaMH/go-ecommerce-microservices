// Package integration exercises the inventory service end to end against a
// real PostgreSQL (testcontainers) and an in-process NATS JetStream server:
// command in, state change in the database, reply event out.
//
// Tests are skipped with -short or when Docker is not available.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// outcomes records the replies observed on the event subjects, by order ID.
type outcomes struct {
	mu       sync.Mutex
	reserved map[string]int
	rejected map[string]*inventoryv1.StockRejected
	released map[string]int
	corr     map[string]string // order ID -> correlation ID header of its reply
}

func (o *outcomes) count() (reserved, rejected, released int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.reserved), len(o.rejected), len(o.released)
}

type env struct {
	pool *pgxpool.Pool
	svc  *app.Service
	pub  *messaging.JetStreamPublisher
	got  *outcomes
}

// newEnv starts the whole stack and tears it down with the test.
func newEnv(t *testing.T) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// PostgreSQL
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("inventory"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
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

	// NATS JetStream
	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, js, err := messaging.Connect(ns.ClientURL(), "inventory-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	stream, err := messaging.EnsureStream(ctx, js, subjects.InventoryStream)
	if err != nil {
		t.Fatal(err)
	}

	// Same wiring as cmd/inventory/main.go.
	store := pgstore.New(pool)
	repo := pgadapter.NewRepository(store)
	svc := app.NewService(repo, repo, natsadapter.NewEvents(store))
	pub := messaging.NewJetStreamPublisher(js)

	go func() {
		_ = outbox.NewRelay(store, pub, discard, outbox.Options{PollInterval: 20 * time.Millisecond}).Run(ctx)
	}()
	go func() {
		_ = natsadapter.Consume(ctx, js, store, natsadapter.NewHandlers(svc), discard)
	}()

	// Observe replies like the order-service would.
	got := &outcomes{
		reserved: map[string]int{}, rejected: map[string]*inventoryv1.StockRejected{},
		released: map[string]int{}, corr: map[string]string{},
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubjects: []string{subjects.InventoryEvtReserved, subjects.InventoryEvtRejected, subjects.InventoryEvtReleased},
		AckPolicy:      jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		got.mu.Lock()
		defer got.mu.Unlock()
		switch m.Subject() {
		case subjects.InventoryEvtReserved:
			var e inventoryv1.StockReserved
			_ = proto.Unmarshal(m.Data(), &e)
			got.reserved[e.GetOrderId()]++
			got.corr[e.GetOrderId()] = m.Headers().Get(messaging.HeaderCorrelationID)
		case subjects.InventoryEvtRejected:
			var e inventoryv1.StockRejected
			_ = proto.Unmarshal(m.Data(), &e)
			got.rejected[e.GetOrderId()] = &e
		case subjects.InventoryEvtReleased:
			var e inventoryv1.StockReleased
			_ = proto.Unmarshal(m.Data(), &e)
			got.released[e.GetOrderId()]++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cc.Stop)

	return &env{pool: pool, svc: svc, pub: pub, got: got}
}

func (e *env) seed(t *testing.T, sku string, onHand int) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO stock_items (sku, name, currency_code, unit_price_minor, on_hand) VALUES ($1, $1, 'USD', 100, $2)`, sku, onHand)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) stock(t *testing.T, sku string) domain.Item {
	t.Helper()
	it, err := e.svc.GetItem(context.Background(), sku)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) send(t *testing.T, subject string, msg proto.Message, corr string) {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	m := messaging.Message{ID: uuid.NewString(), Subject: subject, Data: data}
	if corr != "" {
		m.Headers = map[string]string{messaging.HeaderCorrelationID: corr}
	}
	if err := e.pub.Publish(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func reserveCmd(orderID string, lines ...*commonv1.LineItem) *inventoryv1.ReserveStock {
	return &inventoryv1.ReserveStock{OrderId: orderID, Items: lines}
}

func line(sku string, qty int32) *commonv1.LineItem {
	return &commonv1.LineItem{Sku: sku, Quantity: qty}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMigrateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	if err := pgadapter.Migrate(context.Background(), e.pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestSeedDemoDataIsIdempotentAndKeepsExistingRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := pgadapter.SeedDemoData(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE stock_items SET on_hand = 1 WHERE sku = 'STICKER-NATS-001'`); err != nil {
		t.Fatal(err)
	}
	if err := pgadapter.SeedDemoData(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if got := e.stock(t, "STICKER-NATS-001").OnHand; got != 1 {
		t.Fatalf("seeding overwrote an existing row: on_hand = %d", got)
	}
}

func TestReserveCommandReservesStockAndRepliesWithCorrelationID(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "A", 10)
	e.seed(t, "B", 5)

	e.send(t, subjects.InventoryCmdReserve, reserveCmd("o-1", line("A", 3), line("B", 5)), "corr-7")

	eventually(t, "StockReserved", func() bool { r, _, _ := e.got.count(); return r == 1 })
	if a, b := e.stock(t, "A"), e.stock(t, "B"); a.Reserved != 3 || b.Reserved != 5 || b.Available() != 0 {
		t.Fatalf("stock A=%+v B=%+v", a, b)
	}
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	if e.got.corr["o-1"] != "corr-7" {
		t.Errorf("correlation ID on reply = %q, want corr-7", e.got.corr["o-1"])
	}
}

func TestInsufficientStockIsRejectedAndNothingChanges(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "A", 10)
	e.seed(t, "B", 1)

	e.send(t, subjects.InventoryCmdReserve, reserveCmd("o-1", line("A", 3), line("B", 2), line("ZZZ", 1)), "")

	eventually(t, "StockRejected", func() bool { _, r, _ := e.got.count(); return r == 1 })
	e.got.mu.Lock()
	rej := e.got.rejected["o-1"]
	e.got.mu.Unlock()
	if got := rej.GetUnavailableSkus(); len(got) != 2 || got[0] != "B" || got[1] != "ZZZ" {
		t.Errorf("unavailable SKUs = %v, want [B ZZZ]", got)
	}
	if e.stock(t, "A").Reserved != 0 || e.stock(t, "B").Reserved != 0 {
		t.Error("a rejected reservation must not hold any stock")
	}
}

func TestRepeatedCommandForSameOrderDoesNotReserveTwice(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "A", 10)

	// Different message IDs, same order: the saga re-sending the command.
	for range 3 {
		e.send(t, subjects.InventoryCmdReserve, reserveCmd("o-1", line("A", 4)), "")
	}

	eventually(t, "3 replies handled", func() bool {
		var n int
		_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM inbox`).Scan(&n)
		return n == 3
	})
	if got := e.stock(t, "A").Reserved; got != 4 {
		t.Fatalf("Reserved = %d, want 4 (idempotent per order)", got)
	}
}

func TestReleaseCommandGivesStockBackAndConfirms(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "A", 10)

	e.send(t, subjects.InventoryCmdReserve, reserveCmd("o-1", line("A", 6)), "")
	eventually(t, "StockReserved", func() bool { r, _, _ := e.got.count(); return r == 1 })

	e.send(t, subjects.InventoryCmdRelease, &inventoryv1.ReleaseStock{OrderId: "o-1"}, "")
	eventually(t, "StockReleased", func() bool { _, _, r := e.got.count(); return r == 1 })

	if got := e.stock(t, "A"); got.Reserved != 0 || got.Available() != 10 {
		t.Fatalf("stock after release = %+v", got)
	}
}

func TestMalformedCommandIsTerminatedNotRetried(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "A", 10)

	// An empty order ID is permanently invalid; a following valid command must still be processed.
	e.send(t, subjects.InventoryCmdReserve, reserveCmd("", line("A", 1)), "")
	e.send(t, subjects.InventoryCmdReserve, reserveCmd("o-ok", line("A", 1)), "")

	eventually(t, "valid command after the bad one", func() bool { r, _, _ := e.got.count(); return r == 1 })
	if got := e.stock(t, "A").Reserved; got != 1 {
		t.Fatalf("Reserved = %d, want 1", got)
	}
}

// Many orders compete for few units at once. Row locking must make sure
// exactly the available quantity is handed out, never more.
func TestConcurrentReservationsNeverOversell(t *testing.T) {
	e := newEnv(t)
	const units, orders = 5, 25
	e.seed(t, "HOT", units)

	var wg sync.WaitGroup
	for i := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.svc.Reserve(context.Background(), fmt.Sprintf("order-%02d", i), []domain.Line{{SKU: "HOT", Quantity: 1}})
			if err != nil {
				t.Errorf("order %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if got := e.stock(t, "HOT"); got.Reserved != units || got.Available() != 0 {
		t.Fatalf("stock = %+v, want exactly %d reserved", got, units)
	}
	var reservations int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != units {
		t.Fatalf("%d reservations, want %d", reservations, units)
	}

	// Every order got exactly one answer through the outbox, delivered to the broker.
	eventually(t, "all replies published", func() bool {
		r, rej, _ := e.got.count()
		return r+rej == orders
	})
	if r, rej, _ := e.got.count(); r != units || rej != orders-units {
		t.Fatalf("reserved=%d rejected=%d, want %d and %d", r, rej, units, orders-units)
	}
}

func TestListItemsPagination(t *testing.T) {
	e := newEnv(t)
	for _, sku := range []string{"E", "A", "D", "B", "C"} {
		e.seed(t, sku, 1)
	}

	var all []string
	after := ""
	for range 10 {
		items, next, err := e.svc.ListItems(context.Background(), 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			all = append(all, it.SKU)
		}
		if next == "" {
			break
		}
		after = next
	}
	if fmt.Sprint(all) != "[A B C D E]" {
		t.Fatalf("paged SKUs = %v, want [A B C D E]", all)
	}
}
