// Package integration exercises the notification service end to end against a
// real PostgreSQL (testcontainers) and an in-process NATS JetStream server:
// order event in, notifications recorded and "sent", visible through the API.
//
// The recipient is the customer ID, so the simulated sender's behaviour is
// chosen by the data: "bounce-*" is undeliverable, "flaky-*" fails once.
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
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/sender/simulated"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// countingSender counts every delivery attempt and every successful delivery.
type countingSender struct {
	inner     app.Sender
	attempts  atomic.Int32
	delivered atomic.Int32
}

func (c *countingSender) Send(ctx context.Context, n domain.Notification) error {
	c.attempts.Add(1)
	err := c.inner.Send(ctx, n)
	if err == nil {
		c.delivered.Add(1)
	}
	return err
}

type env struct {
	pool   *pgxpool.Pool
	svc    *app.Service
	pub    *messaging.JetStreamPublisher
	sender *countingSender
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
		tcpostgres.WithDatabase("notifications"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
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
	nc, js, err := messaging.Connect(ns.ClientURL(), "notification-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if _, err := messaging.EnsureStream(ctx, js, subjects.OrderStream); err != nil {
		t.Fatal(err)
	}

	// Same wiring as cmd/notification/main.go, with a counting sender.
	sender := &countingSender{inner: simulated.New(discard)}
	svc := app.NewService(pgadapter.NewRepository(pool), sender, discard, uuid.NewString, func() time.Time { return time.Now().UTC() })
	go func() {
		// A short retry delay so the "flaky gateway" test does not take seconds.
		_ = messaging.Consume(ctx, js, messaging.ConsumerConfig{
			Stream: subjects.OrderStream.Name, Durable: "notification",
			FilterSubjects: []string{subjects.OrderEvtConfirmed, subjects.OrderEvtCancelled},
			RetryDelay:     50 * time.Millisecond,
		}, discard, natsadapter.NewHandlers(svc).Handle)
	}()

	return &env{pool: pool, svc: svc, pub: messaging.NewJetStreamPublisher(js), sender: sender}
}

var ctx = context.Background()

func (e *env) publish(t *testing.T, subject string, msg proto.Message) {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pub.Publish(ctx, messaging.Message{ID: uuid.NewString(), Subject: subject, Data: data}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) confirmed(t *testing.T, orderID, customer string, total int64) {
	t.Helper()
	e.publish(t, subjects.OrderEvtConfirmed, &orderv1.OrderConfirmed{
		OrderId: orderID, CustomerId: customer, Total: &commonv1.Money{CurrencyCode: "USD", AmountMinor: total},
	})
}

func (e *env) cancelled(t *testing.T, orderID, customer, reason string) {
	t.Helper()
	e.publish(t, subjects.OrderEvtCancelled, &orderv1.OrderCancelled{OrderId: orderID, CustomerId: customer, Reason: reason})
}

func (e *env) forOrder(t *testing.T, orderID string) []domain.Notification {
	t.Helper()
	items, _, err := e.svc.ListNotifications(ctx, orderID, "", 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// waitFinal waits until the order has want notifications and none is pending.
func (e *env) waitFinal(t *testing.T, orderID string, want int) []domain.Notification {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		items := e.forOrder(t, orderID)
		pending := 0
		for _, n := range items {
			if n.Status == domain.StatusPending {
				pending++
			}
		}
		if len(items) == want && pending == 0 {
			return items
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("order %s: want %d final notifications, have %+v", orderID, want, e.forOrder(t, orderID))
	return nil
}

func byChannel(ns []domain.Notification) map[domain.Channel]domain.Notification {
	out := map[domain.Channel]domain.Notification{}
	for _, n := range ns {
		out[n.Channel] = n
	}
	return out
}

func TestMigrateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	if err := pgadapter.Migrate(ctx, e.pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestConfirmedOrderSendsAnEmail(t *testing.T) {
	e := newEnv(t)

	e.confirmed(t, "o-1", "alice", 3000)

	ns := e.waitFinal(t, "o-1", 1)
	n := ns[0]
	if n.Channel != domain.ChannelEmail || n.Kind != domain.KindOrderConfirmed || n.Status != domain.StatusSent ||
		n.Recipient != "alice" || n.CustomerID != "alice" || n.Subject == "" {
		t.Fatalf("notification = %+v", n)
	}
	if want := "30.00 USD"; !contains(n.Body, want) || !contains(n.Body, "o-1") {
		t.Errorf("body %q should mention the order and %s", n.Body, want)
	}
	if !n.UpdatedAt.After(n.CreatedAt) && !n.UpdatedAt.Equal(n.CreatedAt) {
		t.Errorf("timestamps = %v / %v", n.CreatedAt, n.UpdatedAt)
	}
}

func TestCancelledOrderSendsEmailAndSMSWithTheReason(t *testing.T) {
	e := newEnv(t)

	e.cancelled(t, "o-1", "alice", "payment failed: card declined")

	got := byChannel(e.waitFinal(t, "o-1", 2))
	for _, ch := range []domain.Channel{domain.ChannelEmail, domain.ChannelSMS} {
		n, ok := got[ch]
		if !ok || n.Status != domain.StatusSent || n.Kind != domain.KindOrderCancelled {
			t.Fatalf("%s notification = %+v", ch, n)
		}
		if !contains(n.Body, "payment") {
			t.Errorf("%s body should explain the payment problem: %q", ch, n.Body)
		}
	}
}

func TestRepeatedEventsNeverNotifyTwice(t *testing.T) {
	e := newEnv(t)

	// The same event many times under different message IDs, as redelivery
	// or an upstream retry would produce.
	for range 5 {
		e.cancelled(t, "o-1", "alice", "saga timeout")
	}
	// A marker event proves the repeats (published earlier) were processed.
	e.confirmed(t, "marker", "alice", 100)
	e.waitFinal(t, "marker", 1)
	e.waitFinal(t, "o-1", 2)

	if got := e.sender.delivered.Load(); got != 3 { // 2 for o-1, 1 for the marker
		t.Fatalf("%d messages delivered, want 3: every notification exactly once", got)
	}
	var rows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE order_id = 'o-1'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows = %d, err = %v; want 2", rows, err)
	}
}

func TestUndeliverableRecipientIsRecordedAsFailedAndNotRetried(t *testing.T) {
	e := newEnv(t)

	e.cancelled(t, "o-1", "bounce-bob", "out of stock: insufficient stock")

	got := byChannel(e.waitFinal(t, "o-1", 2))
	if n := got[domain.ChannelEmail]; n.Status != domain.StatusFailed || n.FailureReason != "mailbox does not exist" {
		t.Errorf("email = %+v", n)
	}
	if n := got[domain.ChannelSMS]; n.Status != domain.StatusFailed || n.FailureReason != "number not reachable" {
		t.Errorf("sms = %+v", n)
	}

	// Let any wrongly scheduled retry happen, then check there was none.
	time.Sleep(400 * time.Millisecond)
	if n := e.sender.attempts.Load(); n != 2 {
		t.Fatalf("%d delivery attempts, want 2: an undeliverable message must not be retried", n)
	}
}

func TestFlakyGatewayIsRetriedUntilEverythingIsDelivered(t *testing.T) {
	e := newEnv(t)

	e.cancelled(t, "o-1", "flaky-carol", "saga timeout")

	got := e.waitFinal(t, "o-1", 2)
	for _, n := range got {
		if n.Status != domain.StatusSent {
			t.Fatalf("notification = %+v, want it sent after the retry", n)
		}
	}
	if d := e.sender.delivered.Load(); d != 2 {
		t.Errorf("%d delivered, want 2 (each channel once)", d)
	}
	if a := e.sender.attempts.Load(); a < 4 {
		t.Errorf("%d attempts, want at least 4 (a failure and a success for each channel)", a)
	}
}

func TestBadMessagesDoNotBlockTheQueue(t *testing.T) {
	e := newEnv(t)

	// Garbage and an event without a customer are permanent failures.
	if err := e.pub.Publish(ctx, messaging.Message{ID: uuid.NewString(), Subject: subjects.OrderEvtConfirmed, Data: []byte{0xff, 0xff, 0xff}}); err != nil {
		t.Fatal(err)
	}
	e.cancelled(t, "no-customer", "", "x")
	e.confirmed(t, "o-ok", "alice", 500)

	e.waitFinal(t, "o-ok", 1)
	if n := len(e.forOrder(t, "no-customer")); n != 0 {
		t.Errorf("%d notifications recorded for an event without a customer", n)
	}
}

// Several workers handling the same event at once must still converge on
// one row per notification, all delivered. Delivery is at-least-once, so a
// message may be sent more than once in such a race, but never fewer times
// than needed and never to the wrong state.
func TestConcurrentHandlingOfOneEventConverges(t *testing.T) {
	e := newEnv(t)
	const workers = 10

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got := e.waitFinal(t, "o-1", 2)
	for _, n := range got {
		if n.Status != domain.StatusSent {
			t.Fatalf("notification = %+v", n)
		}
	}
	if d := e.sender.delivered.Load(); d < 2 || d > 2*workers {
		t.Fatalf("%d delivered, want between 2 and %d", d, 2*workers)
	}
}

func TestListFiltersAndPaginatesNewestFirst(t *testing.T) {
	e := newEnv(t)

	for i := range 3 {
		e.confirmed(t, fmt.Sprintf("alice-%d", i), "alice", 100)
		e.waitFinal(t, fmt.Sprintf("alice-%d", i), 1)
		time.Sleep(5 * time.Millisecond)
	}
	e.confirmed(t, "bob-0", "bob", 100)
	e.waitFinal(t, "bob-0", 1)

	var got []string
	var after *app.Cursor
	for range 10 {
		page, next, err := e.svc.ListNotifications(ctx, "", "alice", 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page {
			got = append(got, n.OrderID)
		}
		if next == nil {
			break
		}
		after = next
	}
	if fmt.Sprint(got) != "[alice-2 alice-1 alice-0]" {
		t.Fatalf("paged = %v, want newest first and only alice's", got)
	}

	all, _, err := e.svc.ListNotifications(ctx, "", "", 50, nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("all: %d, err %v; want 4", len(all), err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
