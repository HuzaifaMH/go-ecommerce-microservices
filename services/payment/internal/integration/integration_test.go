// Package integration exercises the payment service end to end against a
// real PostgreSQL (testcontainers) and an in-process NATS JetStream server:
// command in, payment recorded, reply event out.
//
// Tests are skipped with -short or when Docker is not available.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/adapters/provider/simulated"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// countingProvider counts how often the real provider is reached.
type countingProvider struct {
	app.Provider
	calls atomic.Int32
}

func (c *countingProvider) Charge(ctx context.Context, req domain.ChargeRequest) (string, error) {
	c.calls.Add(1)
	return c.Provider.Charge(ctx, req)
}

type outcomes struct {
	mu        sync.Mutex
	succeeded map[string][]string // order ID -> payment IDs seen
	failed    map[string][]string // order ID -> reasons seen
	corr      map[string]string
}

func (o *outcomes) counts(orderID string) (succeeded, failed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.succeeded[orderID]), len(o.failed[orderID])
}

type env struct {
	pool     *pgxpool.Pool
	svc      *app.Service
	pub      *messaging.JetStreamPublisher
	provider *countingProvider
	got      *outcomes
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

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("payments"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
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

	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, js, err := messaging.Connect(ns.ClientURL(), "payment-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	stream, err := messaging.EnsureStream(ctx, js, subjects.PaymentStream)
	if err != nil {
		t.Fatal(err)
	}

	// Same wiring as cmd/payment/main.go.
	store := pgstore.New(pool)
	repo := pgadapter.NewRepository(store)
	provider := &countingProvider{Provider: simulated.New(1_000_000)}
	svc := app.NewService(provider, repo, repo, natsadapter.NewEvents(store), uuid.NewString)
	pub := messaging.NewJetStreamPublisher(js)

	go func() {
		_ = outbox.NewRelay(store, pub, discard, outbox.Options{PollInterval: 20 * time.Millisecond}).Run(ctx)
	}()
	go func() {
		// Short retry delay so the "flaky provider" test does not take seconds.
		_ = messaging.Consume(ctx, js, messaging.ConsumerConfig{
			Stream: subjects.PaymentStream.Name, Durable: "payment",
			FilterSubjects: []string{subjects.PaymentCmdCharge}, RetryDelay: 50 * time.Millisecond,
		}, discard, natsadapter.NewHandlers(svc).Handle)
	}()

	got := &outcomes{succeeded: map[string][]string{}, failed: map[string][]string{}, corr: map[string]string{}}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubjects: []string{subjects.PaymentEvtSucceeded, subjects.PaymentEvtFailed},
		AckPolicy:      jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		got.mu.Lock()
		defer got.mu.Unlock()
		switch m.Subject() {
		case subjects.PaymentEvtSucceeded:
			var e paymentv1.PaymentSucceeded
			_ = proto.Unmarshal(m.Data(), &e)
			got.succeeded[e.GetOrderId()] = append(got.succeeded[e.GetOrderId()], e.GetPaymentId())
			got.corr[e.GetOrderId()] = m.Headers().Get(messaging.HeaderCorrelationID)
		case subjects.PaymentEvtFailed:
			var e paymentv1.PaymentFailed
			_ = proto.Unmarshal(m.Data(), &e)
			got.failed[e.GetOrderId()] = append(got.failed[e.GetOrderId()], e.GetReason())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cc.Stop)

	return &env{pool: pool, svc: svc, pub: pub, provider: provider, got: got}
}

func (e *env) charge(t *testing.T, orderID, customer string, amountMinor int64, corr string) {
	t.Helper()
	data, err := proto.Marshal(&paymentv1.ChargePayment{
		OrderId: orderID, CustomerId: customer,
		Amount: &commonv1.Money{CurrencyCode: "USD", AmountMinor: amountMinor},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := messaging.Message{ID: uuid.NewString(), Subject: subjects.PaymentCmdCharge, Data: data}
	if corr != "" {
		m.Headers = map[string]string{messaging.HeaderCorrelationID: corr}
	}
	if err := e.pub.Publish(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func (e *env) paymentRows(t *testing.T, orderID string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE order_id = $1`, orderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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

func TestSuccessfulChargeIsRecordedAndReported(t *testing.T) {
	e := newEnv(t)

	e.charge(t, "o-1", "alice", 1999, "corr-5")

	eventually(t, "PaymentSucceeded", func() bool { s, _ := e.got.counts("o-1"); return s == 1 })
	p, err := e.svc.GetPayment(context.Background(), "o-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != domain.StatusSucceeded || p.Amount.AmountMinor != 1999 || p.ProviderRef != "sim_o-1" || p.CreatedAt.IsZero() {
		t.Fatalf("payment = %+v", p)
	}
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	if e.got.succeeded["o-1"][0] != p.ID {
		t.Errorf("event carries payment %q, stored payment is %q", e.got.succeeded["o-1"][0], p.ID)
	}
	if e.got.corr["o-1"] != "corr-5" {
		t.Errorf("correlation ID on reply = %q, want corr-5", e.got.corr["o-1"])
	}
}

func TestDeclinedChargeIsRecordedAndReportedAsFailure(t *testing.T) {
	e := newEnv(t)

	e.charge(t, "o-1", "decline-bob", 500, "")

	eventually(t, "PaymentFailed", func() bool { _, f := e.got.counts("o-1"); return f == 1 })
	p, err := e.svc.GetPayment(context.Background(), "o-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != domain.StatusFailed || p.FailureReason != "card declined" {
		t.Fatalf("payment = %+v", p)
	}
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	if e.got.failed["o-1"][0] != "card declined" {
		t.Errorf("reason on event = %q", e.got.failed["o-1"][0])
	}
}

func TestAmountOverTheLimitIsDeclined(t *testing.T) {
	e := newEnv(t)
	e.charge(t, "o-1", "alice", 1_000_001, "")
	eventually(t, "PaymentFailed", func() bool { _, f := e.got.counts("o-1"); return f == 1 })
}

func TestRepeatedChargeNeverChargesTwice(t *testing.T) {
	e := newEnv(t)

	// Different message IDs, same order: the saga re-sending the command.
	for range 3 {
		e.charge(t, "o-1", "alice", 1999, "")
	}

	eventually(t, "three replies", func() bool { s, _ := e.got.counts("o-1"); return s == 3 })
	if n := e.provider.calls.Load(); n != 1 {
		t.Fatalf("provider reached %d times, want 1", n)
	}
	if n := e.paymentRows(t, "o-1"); n != 1 {
		t.Fatalf("%d payment rows, want 1", n)
	}
	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	ids := e.got.succeeded["o-1"]
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Errorf("every reply must reference the same payment: %v", ids)
	}
}

func TestProviderOutageIsRetriedUntilItSucceeds(t *testing.T) {
	e := newEnv(t)

	e.charge(t, "o-1", "flaky-carol", 500, "")

	eventually(t, "PaymentSucceeded after a retry", func() bool { s, _ := e.got.counts("o-1"); return s >= 1 })
	if n := e.provider.calls.Load(); n < 2 {
		t.Fatalf("provider reached %d times, want at least 2 (outage, then success)", n)
	}
	if n := e.paymentRows(t, "o-1"); n != 1 {
		t.Fatalf("%d payment rows, want 1", n)
	}
}

func TestConflictingCommandForSameOrderIsTerminatedAndOriginalStands(t *testing.T) {
	e := newEnv(t)
	e.charge(t, "o-1", "alice", 1999, "")
	eventually(t, "first charge", func() bool { s, _ := e.got.counts("o-1"); return s == 1 })

	e.charge(t, "o-1", "alice", 5000, "") // same order, different amount
	e.charge(t, "o-2", "alice", 100, "")  // a later, valid command must still be processed

	eventually(t, "later command after the conflicting one", func() bool { s, _ := e.got.counts("o-2"); return s == 1 })
	p, _ := e.svc.GetPayment(context.Background(), "o-1")
	if p.Amount.AmountMinor != 1999 {
		t.Fatalf("original payment changed: %+v", p)
	}
	if s, _ := e.got.counts("o-1"); s != 1 {
		t.Errorf("a conflicting command must not produce another reply, got %d", s)
	}
}

// Many workers charge the same order at once. Whatever the interleaving,
// exactly one payment may exist and every reply must agree on it.
func TestConcurrentChargesOfOneOrderProduceOnePayment(t *testing.T) {
	e := newEnv(t)
	const attempts = 12

	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.svc.Charge(context.Background(), "o-1", "alice", domain.Money{CurrencyCode: "USD", AmountMinor: 700})
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if n := e.paymentRows(t, "o-1"); n != 1 {
		t.Fatalf("%d payment rows, want 1", n)
	}
	eventually(t, "all replies published", func() bool { s, _ := e.got.counts("o-1"); return s == attempts })

	e.got.mu.Lock()
	defer e.got.mu.Unlock()
	first := e.got.succeeded["o-1"][0]
	for _, id := range e.got.succeeded["o-1"] {
		if id != first {
			t.Fatalf("replies disagree on the payment: %v", e.got.succeeded["o-1"])
		}
	}
}

func TestManyIndependentOrders(t *testing.T) {
	e := newEnv(t)
	const orders = 20
	for i := range orders {
		customer := "alice"
		if i%4 == 0 {
			customer = "decline-bob"
		}
		e.charge(t, fmt.Sprintf("order-%02d", i), customer, 100, "")
	}

	eventually(t, "every order answered", func() bool {
		done := 0
		for i := range orders {
			s, f := e.got.counts(fmt.Sprintf("order-%02d", i))
			if s+f == 1 {
				done++
			}
		}
		return done == orders
	})
	var failed int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE status = 'failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != orders/4 {
		t.Fatalf("%d failed payments, want %d", failed, orders/4)
	}
}
