//go:build e2e

// Package e2e runs the whole system as a black box: it talks to the real
// order, inventory and payment services started by Docker Compose, over their
// public gRPC APIs only (no internal packages).
//
//	make e2e
//
// or by hand:
//
//	docker compose -f deploy/docker-compose.yml up -d --build
//	go test -tags e2e -count=1 ./test/e2e/...
//
// Addresses can be overridden with E2E_ORDER_ADDR, E2E_INVENTORY_ADDR and
// E2E_PAYMENT_ADDR. The tests use the demo catalogue the inventory service
// seeds (SEED_DEMO_DATA=true) and the simulated payment provider, where a
// customer ID starting with "decline-" is declined.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	paymentv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/payment/v1"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// system holds clients for the three services.
type system struct {
	orders        orderv1.OrderServiceClient
	inventory     inventoryv1.InventoryServiceClient
	payments      paymentv1.PaymentServiceClient
	notifications notificationv1.NotificationServiceClient
}

func connect(t *testing.T) *system {
	t.Helper()
	dial := func(addr string) *grpc.ClientConn {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	waitReady(t, env("E2E_ORDER_HEALTH", "http://localhost:8083/readyz"))
	waitReady(t, env("E2E_INVENTORY_HEALTH", "http://localhost:8081/readyz"))
	waitReady(t, env("E2E_PAYMENT_HEALTH", "http://localhost:8082/readyz"))
	waitReady(t, env("E2E_NOTIFICATION_HEALTH", "http://localhost:8084/readyz"))

	return &system{
		orders:        orderv1.NewOrderServiceClient(dial(env("E2E_ORDER_ADDR", "localhost:9092"))),
		inventory:     inventoryv1.NewInventoryServiceClient(dial(env("E2E_INVENTORY_ADDR", "localhost:9090"))),
		payments:      paymentv1.NewPaymentServiceClient(dial(env("E2E_PAYMENT_ADDR", "localhost:9091"))),
		notifications: notificationv1.NewNotificationServiceClient(dial(env("E2E_NOTIFICATION_ADDR", "localhost:9093"))),
	}
}

// waitReady polls a readiness endpoint until it answers 200, or fails the test.
func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s not ready: %s (is the stack running? try `make up`)", url, last)
}

var bg = context.Background()

func (s *system) create(t *testing.T, customer, key string, items ...*orderv1.OrderItem) (*orderv1.Order, error) {
	t.Helper()
	resp, err := s.orders.CreateOrder(bg, &orderv1.CreateOrderRequest{CustomerId: customer, IdempotencyKey: key, Items: items})
	if err != nil {
		return nil, err
	}
	return resp.GetOrder(), nil
}

func item(sku string, qty int32) *orderv1.OrderItem {
	return &orderv1.OrderItem{Sku: sku, Quantity: qty}
}

// waitStatus polls GetOrder until the order reaches want.
func (s *system) waitStatus(t *testing.T, id string, want orderv1.OrderStatus) *orderv1.Order {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last *orderv1.Order
	for time.Now().Before(deadline) {
		resp, err := s.orders.GetOrder(bg, &orderv1.GetOrderRequest{Id: id})
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		last = resp.GetOrder()
		if last.GetStatus() == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("order %s is %s, want %s (reason %q)", id, last.GetStatus(), want, last.GetCancelReason())
	return nil
}

func (s *system) stock(t *testing.T, sku string) *inventoryv1.StockItem {
	t.Helper()
	resp, err := s.inventory.GetStock(bg, &inventoryv1.GetStockRequest{Sku: sku})
	if err != nil {
		t.Fatalf("GetStock(%s): %v", sku, err)
	}
	return resp.GetItem()
}

// waitReserved polls until the SKU's reserved quantity equals want.
func (s *system) waitReserved(t *testing.T, sku string, want int32) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s.stock(t, sku).GetReserved() == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s reserved = %d, want %d", sku, s.stock(t, sku).GetReserved(), want)
}

// waitNotifications polls until the order has want notifications, none of them pending.
func (s *system) waitNotifications(t *testing.T, orderID string, want int) map[notificationv1.NotificationChannel]*notificationv1.Notification {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last []*notificationv1.Notification
	for time.Now().Before(deadline) {
		resp, err := s.notifications.ListNotifications(bg, &notificationv1.ListNotificationsRequest{OrderId: orderID})
		if err != nil {
			t.Fatalf("ListNotifications: %v", err)
		}
		last = resp.GetNotifications()
		pending := 0
		for _, n := range last {
			if n.GetStatus() == notificationv1.NotificationStatus_NOTIFICATION_STATUS_PENDING {
				pending++
			}
		}
		if len(last) == want && pending == 0 {
			out := map[notificationv1.NotificationChannel]*notificationv1.Notification{}
			for _, n := range last {
				out[n.GetChannel()] = n
			}
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("order %s: want %d final notifications, have %v", orderID, want, last)
	return nil
}

func unique(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestHappyPath(t *testing.T) {
	s := connect(t)
	const sku = "MUG-GOPHER-001" // $12.99, plenty in stock
	before := s.stock(t, sku)

	o, err := s.create(t, unique("alice"), unique("key"), item(sku, 2))
	if err != nil {
		t.Fatal(err)
	}
	if o.GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PENDING {
		t.Fatalf("a new order must be pending, got %s", o.GetStatus())
	}
	if got := o.GetTotal().GetAmountMinor(); got != 2*1299 {
		t.Errorf("total = %d, want %d (priced from the catalogue)", got, 2*1299)
	}

	s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CONFIRMED)

	if got := s.stock(t, sku).GetReserved(); got != before.GetReserved()+2 {
		t.Errorf("reserved = %d, want %d", got, before.GetReserved()+2)
	}
	p, err := s.payments.GetPayment(bg, &paymentv1.GetPaymentRequest{OrderId: o.GetId()})
	if err != nil {
		t.Fatalf("GetPayment: %v", err)
	}
	if p.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_SUCCEEDED || p.GetPayment().GetAmount().GetAmountMinor() != 2*1299 {
		t.Errorf("payment = %v", p.GetPayment())
	}

	// The customer was told: one email, delivered.
	sent := s.waitNotifications(t, o.GetId(), 1)
	email := sent[notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL]
	if email.GetStatus() != notificationv1.NotificationStatus_NOTIFICATION_STATUS_SENT ||
		email.GetKind() != notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CONFIRMED ||
		email.GetCustomerId() != o.GetCustomerId() || !strings.Contains(email.GetBody(), "25.98 USD") {
		t.Errorf("email = %v", email)
	}
}

func TestUndeliverableCustomerDoesNotAffectTheOrder(t *testing.T) {
	s := connect(t)

	// The simulated sender cannot reach customers whose ID starts with "bounce-".
	o, err := s.create(t, unique("bounce-dave"), unique("key"), item("BOOK-GO-001", 1))
	if err != nil {
		t.Fatal(err)
	}
	s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CONFIRMED)

	got := s.waitNotifications(t, o.GetId(), 1)
	email := got[notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL]
	if email.GetStatus() != notificationv1.NotificationStatus_NOTIFICATION_STATUS_FAILED || email.GetFailureReason() == "" {
		t.Errorf("email = %v, want a recorded delivery failure", email)
	}
	// A notification problem must never undo a paid order.
	if got := s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CONFIRMED); got == nil {
		t.Error("order should stay confirmed")
	}
}

func TestClientPricesAreIgnored(t *testing.T) {
	s := connect(t)
	o, err := s.create(t, unique("mallory"), unique("key"),
		&orderv1.OrderItem{Sku: "STICKER-NATS-001", Quantity: 1, UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.GetTotal().GetAmountMinor(); got != 499 {
		t.Fatalf("total = %d, want 499: the order must be priced by the catalogue, not the client", got)
	}
	// Tidy up so the sticker stock stays available for the out-of-stock test.
	if _, err := s.orders.CancelOrder(bg, &orderv1.CancelOrderRequest{Id: o.GetId(), Reason: "test cleanup"}); err != nil {
		t.Fatal(err)
	}
	s.waitReserved(t, "STICKER-NATS-001", 0)
}

func TestDeclinedPaymentCancelsTheOrderAndReleasesStock(t *testing.T) {
	s := connect(t)
	const sku = "TSHIRT-GO-M"
	before := s.stock(t, sku).GetReserved()

	o, err := s.create(t, unique("decline-bob"), unique("key"), item(sku, 3))
	if err != nil {
		t.Fatal(err)
	}
	done := s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CANCELLED)

	if done.GetCancelReason() != "payment failed: card declined" {
		t.Errorf("reason = %q", done.GetCancelReason())
	}
	s.waitReserved(t, sku, before) // the compensation gave the stock back

	p, err := s.payments.GetPayment(bg, &paymentv1.GetPaymentRequest{OrderId: o.GetId()})
	if err != nil || p.GetPayment().GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED {
		t.Errorf("payment = %v, err = %v; want a recorded failure", p, err)
	}

	// The customer was told why: an email and an SMS, both delivered.
	sent := s.waitNotifications(t, o.GetId(), 2)
	for _, ch := range []notificationv1.NotificationChannel{
		notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL,
		notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_SMS,
	} {
		n := sent[ch]
		if n.GetStatus() != notificationv1.NotificationStatus_NOTIFICATION_STATUS_SENT ||
			n.GetKind() != notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CANCELLED || !strings.Contains(n.GetBody(), "payment") {
			t.Errorf("%s = %v", ch, n)
		}
	}
}

func TestOutOfStockCancelsTheOrderWithoutCharging(t *testing.T) {
	s := connect(t)
	const sku = "STICKER-NATS-001" // only 5 in stock

	o, err := s.create(t, unique("alice"), unique("key"), item(sku, 6))
	if err != nil {
		t.Fatal(err)
	}
	done := s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CANCELLED)

	if !strings.Contains(done.GetCancelReason(), "out of stock") {
		t.Errorf("reason = %q", done.GetCancelReason())
	}
	if s.stock(t, sku).GetReserved() != 0 {
		t.Errorf("a rejected order must hold no stock, reserved = %d", s.stock(t, sku).GetReserved())
	}
	_, err = s.payments.GetPayment(bg, &paymentv1.GetPaymentRequest{OrderId: o.GetId()})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetPayment err = %v, want NotFound: nobody may be charged for an order without stock", err)
	}
}

func TestCreateOrderIsIdempotent(t *testing.T) {
	s := connect(t)
	customer, key := unique("alice"), unique("key")

	first, err := s.create(t, customer, key, item("BOOK-GO-001", 1))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.create(t, customer, key, item("BOOK-GO-001", 1))
	if err != nil {
		t.Fatal(err)
	}
	if again.GetId() != first.GetId() {
		t.Fatalf("a repeated request created order %s, want the original %s", again.GetId(), first.GetId())
	}

	_, err = s.create(t, customer, key, item("BOOK-GO-001", 5)) // same key, different request
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing a key for a different request: %v, want AlreadyExists", err)
	}
}

func TestConfirmedOrderCannotBeCancelled(t *testing.T) {
	s := connect(t)
	o, err := s.create(t, unique("alice"), unique("key"), item("BOOK-GRPC-001", 1))
	if err != nil {
		t.Fatal(err)
	}
	s.waitStatus(t, o.GetId(), orderv1.OrderStatus_ORDER_STATUS_CONFIRMED)

	_, err = s.orders.CancelOrder(bg, &orderv1.CancelOrderRequest{Id: o.GetId(), Reason: "too late"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestValidation(t *testing.T) {
	s := connect(t)

	_, err := s.create(t, unique("alice"), unique("key"), item("NOT-A-SKU", 1))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown SKU: %v, want InvalidArgument", err)
	}
	_, err = s.create(t, unique("alice"), unique("key"), item("BOOK-GO-001", 0))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("zero quantity: %v, want InvalidArgument", err)
	}
	_, err = s.create(t, "", unique("key"), item("BOOK-GO-001", 1))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("no customer: %v, want InvalidArgument", err)
	}
	_, err = s.orders.GetOrder(bg, &orderv1.GetOrderRequest{Id: "does-not-exist"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown order: %v, want NotFound", err)
	}
}

func TestListOrdersForACustomer(t *testing.T) {
	s := connect(t)
	customer := unique("lister")

	var ids []string
	for i := range 3 {
		o, err := s.create(t, customer, fmt.Sprintf("key-%d", i), item("BOOK-GO-001", 1))
		if err != nil {
			t.Fatal(err)
		}
		ids = append([]string{o.GetId()}, ids...) // newest first
		time.Sleep(10 * time.Millisecond)
	}

	var got []string
	token := ""
	for range 5 {
		resp, err := s.orders.ListOrders(bg, &orderv1.ListOrdersRequest{CustomerId: customer, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range resp.GetOrders() {
			got = append(got, o.GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Fatalf("listed %v, want %v", got, ids)
	}
}
