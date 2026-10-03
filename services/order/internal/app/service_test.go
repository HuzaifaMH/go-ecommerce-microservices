package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

// fakeRepo is an in-memory Repository. InTx snapshots state so a failed
// transaction rolls back, like a real database.
type fakeRepo struct {
	orders map[string]domain.Order
	failOn string // "save" makes Save fail
}

func newFakeRepo() *fakeRepo { return &fakeRepo{orders: map[string]domain.Order{}} }

func (r *fakeRepo) InTx(ctx context.Context, fn func(context.Context) error) error {
	snapshot := make(map[string]domain.Order, len(r.orders))
	for k, v := range r.orders {
		snapshot[k] = v
	}
	if err := fn(ctx); err != nil {
		r.orders = snapshot
		return err
	}
	return nil
}

func (r *fakeRepo) Create(_ context.Context, o domain.Order) (domain.Order, bool, error) {
	for _, existing := range r.orders {
		if existing.CustomerID == o.CustomerID && existing.IdempotencyKey == o.IdempotencyKey {
			return existing, false, nil
		}
	}
	r.orders[o.ID] = o
	return o, true, nil
}

func (r *fakeRepo) FindByKey(_ context.Context, customerID, key string) (domain.Order, error) {
	for _, o := range r.orders {
		if o.CustomerID == customerID && o.IdempotencyKey == key {
			return o, nil
		}
	}
	return domain.Order{}, domain.ErrOrderNotFound
}

func (r *fakeRepo) Get(_ context.Context, id string) (domain.Order, error) {
	o, ok := r.orders[id]
	if !ok {
		return domain.Order{}, domain.ErrOrderNotFound
	}
	return o, nil
}

func (r *fakeRepo) GetForUpdate(ctx context.Context, id string) (domain.Order, error) {
	return r.Get(ctx, id)
}

func (r *fakeRepo) Save(_ context.Context, o domain.Order) error {
	if r.failOn == "save" {
		return errors.New("disk full")
	}
	r.orders[o.ID] = o
	return nil
}

func (r *fakeRepo) List(_ context.Context, q ListQuery) ([]domain.Order, error) {
	var all []domain.Order
	for _, o := range r.orders {
		if q.CustomerID != "" && o.CustomerID != q.CustomerID {
			continue
		}
		all = append(all, o)
	}
	sort.Slice(all, func(a, b int) bool {
		if !all[a].CreatedAt.Equal(all[b].CreatedAt) {
			return all[a].CreatedAt.After(all[b].CreatedAt)
		}
		return all[a].ID > all[b].ID
	})
	if q.After != nil {
		var rest []domain.Order
		for _, o := range all {
			if o.CreatedAt.Before(q.After.CreatedAt) || (o.CreatedAt.Equal(q.After.CreatedAt) && o.ID < q.After.ID) {
				rest = append(rest, o)
			}
		}
		all = rest
	}
	if len(all) > q.Limit {
		all = all[:q.Limit]
	}
	return all, nil
}

func (r *fakeRepo) LockStale(_ context.Context, cutoff time.Time, limit int) ([]domain.Order, error) {
	var out []domain.Order
	for _, o := range r.orders {
		if !o.Status.Final() && o.UpdatedAt.Before(cutoff) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type fakeCatalog struct {
	prices map[string]domain.Money
	err    error
	calls  int
}

func (c *fakeCatalog) Prices(_ context.Context, skus []string) (map[string]domain.Money, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]domain.Money{}
	for _, s := range skus {
		p, ok := c.prices[s]
		if !ok {
			return nil, &domain.UnknownSKUError{SKU: s}
		}
		out[s] = p
	}
	return out, nil
}

// sent records every command and event in order.
type sent struct{ log []string }

func (s *sent) ReserveStock(_ context.Context, id string, lines []domain.Line) error {
	s.log = append(s.log, fmt.Sprintf("reserve:%s:%v", id, lines))
	return nil
}
func (s *sent) ReleaseStock(_ context.Context, id string) error {
	s.log = append(s.log, "release:"+id)
	return nil
}
func (s *sent) ChargePayment(_ context.Context, id, customer string, total domain.Money) error {
	s.log = append(s.log, fmt.Sprintf("charge:%s:%s:%d", id, customer, total.AmountMinor))
	return nil
}
func (s *sent) OrderConfirmed(_ context.Context, o domain.Order) error {
	s.log = append(s.log, "confirmed:"+o.ID)
	return nil
}
func (s *sent) OrderCancelled(_ context.Context, o domain.Order) error {
	s.log = append(s.log, fmt.Sprintf("cancelled:%s:%s", o.ID, o.CancelReason))
	return nil
}

// fakeMetrics records what the service reported.
type fakeMetrics struct {
	created  int
	finished []string // "<status>/<cause>"
	refunds  int
}

func (m *fakeMetrics) OrderCreated() { m.created++ }
func (m *fakeMetrics) OrderFinished(o domain.Order) {
	if o.Status == domain.StatusCancelled {
		m.finished = append(m.finished, "cancelled/"+domain.CancelCause(o.CancelReason))
		return
	}
	m.finished = append(m.finished, string(o.Status))
}
func (m *fakeMetrics) RefundRequired() { m.refunds++ }

type harness struct {
	svc     *Service
	repo    *fakeRepo
	catalog *fakeCatalog
	sent    *sent
	clock   *time.Time
	metrics *fakeMetrics
}

func setup() *harness {
	repo := newFakeRepo()
	cat := &fakeCatalog{prices: map[string]domain.Money{
		"A": {CurrencyCode: "USD", AmountMinor: 1000},
		"B": {CurrencyCode: "USD", AmountMinor: 250},
	}}
	s := &sent{}
	clock := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	n := 0
	svc := NewService(Deps{
		Catalog: cat, Repo: repo, Tx: repo, Commands: s, Events: s,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewID: func() string { n++; return fmt.Sprintf("o-%d", n) },
		Now:   func() time.Time { return clock },
	})
	m := &fakeMetrics{}
	svc.WithMetrics(m)
	return &harness{svc: svc, repo: repo, catalog: cat, sent: s, clock: &clock, metrics: m}
}

var ctx = context.Background()

func (h *harness) create(t *testing.T) domain.Order {
	t.Helper()
	o, err := h.svc.CreateOrder(ctx, "c-1", "key-1", []domain.Line{{SKU: "B", Quantity: 4}, {SKU: "A", Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (h *harness) status(id string) domain.Status { return h.repo.orders[id].Status }

func (h *harness) logIs(t *testing.T, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(h.sent.log, want) {
		t.Fatalf("sent = %v\nwant = %v", h.sent.log, want)
	}
}

func TestCreateOrderPricesFromCatalogAndStartsSaga(t *testing.T) {
	h := setup()
	o := h.create(t)

	if o.Status != domain.StatusPending || o.Total.AmountMinor != 3000 { // 2*1000 + 4*250
		t.Fatalf("order = %+v", o)
	}
	h.logIs(t, "reserve:o-1:[{A 2} {B 4}]")
}

func TestCreateOrderIsIdempotentAndSkipsTheCatalogOnRepeat(t *testing.T) {
	h := setup()
	first := h.create(t)
	h.sent.log = nil

	again, err := h.svc.CreateOrder(ctx, "c-1", "key-1", []domain.Line{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || len(h.sent.log) != 0 || h.catalog.calls != 1 {
		t.Fatalf("repeat created or sent something: id=%s sent=%v catalogCalls=%d", again.ID, h.sent.log, h.catalog.calls)
	}
}

func TestCreateOrderRejectsReusedKeyWithDifferentRequest(t *testing.T) {
	h := setup()
	h.create(t)

	_, err := h.svc.CreateOrder(ctx, "c-1", "key-1", []domain.Line{{SKU: "A", Quantity: 99}})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
	// The same key for a different customer is a different request entirely.
	if _, err := h.svc.CreateOrder(ctx, "c-2", "key-1", []domain.Line{{SKU: "A", Quantity: 99}}); err != nil {
		t.Fatalf("keys are scoped per customer: %v", err)
	}
}

func TestCreateOrderErrors(t *testing.T) {
	h := setup()
	lines := []domain.Line{{SKU: "A", Quantity: 1}}

	if _, err := h.svc.CreateOrder(ctx, "", "k", lines); !errors.Is(err, domain.ErrInvalidOrder) {
		t.Errorf("no customer: %v", err)
	}
	if _, err := h.svc.CreateOrder(ctx, "c", "", lines); !errors.Is(err, domain.ErrInvalidOrder) {
		t.Errorf("no key: %v", err)
	}
	if _, err := h.svc.CreateOrder(ctx, "c", "k", nil); !errors.Is(err, domain.ErrInvalidOrder) {
		t.Errorf("no lines: %v", err)
	}
	var unknown *domain.UnknownSKUError
	if _, err := h.svc.CreateOrder(ctx, "c", "k", []domain.Line{{SKU: "ZZZ", Quantity: 1}}); !errors.As(err, &unknown) {
		t.Errorf("unknown sku: %v", err)
	}
	h.catalog.err = errors.New("inventory unreachable")
	if _, err := h.svc.CreateOrder(ctx, "c", "k2", lines); err == nil {
		t.Error("catalog outage must fail the request")
	}
	if len(h.repo.orders) != 0 || len(h.sent.log) != 0 {
		t.Errorf("failed requests must leave nothing behind: %v %v", h.repo.orders, h.sent.log)
	}
}

func TestHappyPathSaga(t *testing.T) {
	h := setup()
	o := h.create(t)

	if err := h.svc.HandleStockReserved(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if h.status(o.ID) != domain.StatusStockReserved {
		t.Fatalf("status = %s", h.status(o.ID))
	}
	if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if h.status(o.ID) != domain.StatusConfirmed {
		t.Fatalf("status = %s", h.status(o.ID))
	}
	h.logIs(t, "reserve:o-1:[{A 2} {B 4}]", "charge:o-1:c-1:3000", "confirmed:o-1")
}

func TestStockRejectedCancelsWithoutReleasing(t *testing.T) {
	h := setup()
	o := h.create(t)
	h.sent.log = nil

	if err := h.svc.HandleStockRejected(ctx, o.ID, "insufficient stock"); err != nil {
		t.Fatal(err)
	}
	if h.status(o.ID) != domain.StatusCancelled {
		t.Fatalf("status = %s", h.status(o.ID))
	}
	h.logIs(t, "cancelled:o-1:out of stock: insufficient stock") // nothing was reserved: no release
}

func TestPaymentFailureCompensatesByReleasingStock(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	h.sent.log = nil

	if err := h.svc.HandlePaymentFailed(ctx, o.ID, "card declined"); err != nil {
		t.Fatal(err)
	}
	if got := h.repo.orders[o.ID]; got.Status != domain.StatusCancelled || got.CancelReason != "payment failed: card declined" {
		t.Fatalf("order = %+v", got)
	}
	h.logIs(t, "release:o-1", "cancelled:o-1:payment failed: card declined")
}

func TestDuplicateRepliesAreIgnored(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	_ = h.svc.HandlePaymentSucceeded(ctx, o.ID)
	h.sent.log = nil

	for range 2 {
		if err := h.svc.HandleStockReserved(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.HandlePaymentFailed(ctx, o.ID, "late"); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.HandleStockRejected(ctx, o.ID, "late"); err != nil {
			t.Fatal(err)
		}
	}
	if h.status(o.ID) != domain.StatusConfirmed || len(h.sent.log) != 0 {
		t.Fatalf("status = %s, sent = %v", h.status(o.ID), h.sent.log)
	}
}

func TestLateReservationForCancelledOrderIsReleased(t *testing.T) {
	h := setup()
	o := h.create(t)
	if _, err := h.svc.CancelOrder(ctx, o.ID, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	h.sent.log = nil

	// The reservation was already in flight when the customer cancelled.
	if err := h.svc.HandleStockReserved(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	h.logIs(t, "release:o-1")
	if h.status(o.ID) != domain.StatusCancelled {
		t.Fatalf("status = %s", h.status(o.ID))
	}
}

func TestPaymentSucceededForCancelledOrderFlagsRefund(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	_, _ = h.svc.CancelOrder(ctx, o.ID, "changed my mind")
	h.sent.log = nil

	for range 2 { // repeating must not change anything further
		if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
	}
	got := h.repo.orders[o.ID]
	if got.Status != domain.StatusCancelled || !got.RefundRequired {
		t.Fatalf("order = %+v, want cancelled with RefundRequired", got)
	}
	if len(h.sent.log) != 0 {
		t.Errorf("nothing should be sent: %v", h.sent.log)
	}
}

func TestEventsInImpossibleStatesAreErrors(t *testing.T) {
	h := setup()
	o := h.create(t) // pending: payment results make no sense yet

	if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("payment succeeded while pending: %v", err)
	}
	if err := h.svc.HandlePaymentFailed(ctx, o.ID, "x"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("payment failed while pending: %v", err)
	}
	if err := h.svc.HandleStockReserved(ctx, "nope"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Errorf("unknown order: %v", err)
	}
}

func TestCancelRules(t *testing.T) {
	t.Run("pending order releases stock and announces", func(t *testing.T) {
		h := setup()
		o := h.create(t)
		h.sent.log = nil

		got, err := h.svc.CancelOrder(ctx, o.ID, "changed my mind")
		if err != nil || got.Status != domain.StatusCancelled {
			t.Fatalf("got %+v, %v", got, err)
		}
		h.logIs(t, "release:o-1", "cancelled:o-1:changed my mind")
	})

	t.Run("stock reserved order can be cancelled too", func(t *testing.T) {
		h := setup()
		o := h.create(t)
		_ = h.svc.HandleStockReserved(ctx, o.ID)
		if _, err := h.svc.CancelOrder(ctx, o.ID, "x"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("confirmed order cannot be cancelled", func(t *testing.T) {
		h := setup()
		o := h.create(t)
		_ = h.svc.HandleStockReserved(ctx, o.ID)
		_ = h.svc.HandlePaymentSucceeded(ctx, o.ID)
		h.sent.log = nil

		if _, err := h.svc.CancelOrder(ctx, o.ID, "x"); !errors.Is(err, domain.ErrCannotCancel) {
			t.Fatalf("err = %v, want ErrCannotCancel", err)
		}
		if h.status(o.ID) != domain.StatusConfirmed || len(h.sent.log) != 0 {
			t.Errorf("a rejected cancel must change nothing: %s %v", h.status(o.ID), h.sent.log)
		}
	})

	t.Run("cancelling twice is idempotent", func(t *testing.T) {
		h := setup()
		o := h.create(t)
		_, _ = h.svc.CancelOrder(ctx, o.ID, "first")
		h.sent.log = nil

		got, err := h.svc.CancelOrder(ctx, o.ID, "second")
		if err != nil || got.CancelReason != "first" || len(h.sent.log) != 0 {
			t.Fatalf("got %+v, %v, sent %v", got, err, h.sent.log)
		}
	})

	t.Run("unknown order", func(t *testing.T) {
		if _, err := setup().svc.CancelOrder(ctx, "nope", "x"); !errors.Is(err, domain.ErrOrderNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestExpireStaleCancelsOnlyOldUnfinishedOrders(t *testing.T) {
	h := setup()
	old := h.create(t) // pending, will go stale
	*h.clock = h.clock.Add(time.Minute)
	fresh, _ := h.svc.CreateOrder(ctx, "c-1", "key-2", []domain.Line{{SKU: "A", Quantity: 1}})
	done, _ := h.svc.CreateOrder(ctx, "c-1", "key-3", []domain.Line{{SKU: "A", Quantity: 1}})
	_ = h.svc.HandleStockReserved(ctx, done.ID)
	_ = h.svc.HandlePaymentSucceeded(ctx, done.ID)
	h.sent.log = nil

	*h.clock = h.clock.Add(90 * time.Second) // old: 150s, fresh and done: 90s
	n, err := h.svc.ExpireStale(ctx, 2*time.Minute, 10)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, err %v; want 1", n, err)
	}
	if h.status(old.ID) != domain.StatusCancelled || h.repo.orders[old.ID].CancelReason != "saga timeout" {
		t.Errorf("old order = %+v", h.repo.orders[old.ID])
	}
	if h.status(fresh.ID) != domain.StatusPending || h.status(done.ID) != domain.StatusConfirmed {
		t.Errorf("fresh = %s, done = %s", h.status(fresh.ID), h.status(done.ID))
	}
	h.logIs(t, "release:"+old.ID, "cancelled:"+old.ID+":saga timeout")

	if n, _ := h.svc.ExpireStale(ctx, 2*time.Minute, 10); n != 0 {
		t.Errorf("a second sweep expired %d orders, want 0", n)
	}
}

func TestExpireStaleRollsBackOnFailure(t *testing.T) {
	h := setup()
	h.create(t)
	*h.clock = h.clock.Add(time.Hour)
	h.repo.failOn = "save"

	if _, err := h.svc.ExpireStale(ctx, time.Minute, 10); err == nil {
		t.Fatal("expected error")
	}
	if h.status("o-1") != domain.StatusPending {
		t.Errorf("status = %s; the failed sweep must roll back", h.status("o-1"))
	}
}

func TestListOrdersPaginatesNewestFirst(t *testing.T) {
	h := setup()
	var ids []string
	for i := range 5 {
		*h.clock = h.clock.Add(time.Second)
		o, err := h.svc.CreateOrder(ctx, "c-1", fmt.Sprintf("k-%d", i), []domain.Line{{SKU: "A", Quantity: 1}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append([]string{o.ID}, ids...) // newest first
	}
	_, _ = h.svc.CreateOrder(ctx, "c-2", "other", []domain.Line{{SKU: "A", Quantity: 1}})

	var got []string
	var after *Cursor
	for range 10 {
		page, next, err := h.svc.ListOrders(ctx, "c-1", 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page {
			got = append(got, o.ID)
		}
		if next == nil {
			break
		}
		after = next
	}
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("paged = %v, want %v", got, ids)
	}
}

func TestMetricsCountEachOutcomeExactlyOnce(t *testing.T) {
	h := setup()

	// Confirmed.
	ok := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, ok.ID)
	_ = h.svc.HandlePaymentSucceeded(ctx, ok.ID)

	// Payment declined.
	declined, _ := h.svc.CreateOrder(ctx, "c-1", "key-2", []domain.Line{{SKU: "A", Quantity: 1}})
	_ = h.svc.HandleStockReserved(ctx, declined.ID)
	_ = h.svc.HandlePaymentFailed(ctx, declined.ID, "card declined")

	// Out of stock.
	oos, _ := h.svc.CreateOrder(ctx, "c-1", "key-3", []domain.Line{{SKU: "A", Quantity: 1}})
	_ = h.svc.HandleStockRejected(ctx, oos.ID, "insufficient stock")

	// Cancelled by the customer.
	mine, _ := h.svc.CreateOrder(ctx, "c-1", "key-4", []domain.Line{{SKU: "A", Quantity: 1}})
	_, _ = h.svc.CancelOrder(ctx, mine.ID, "changed my mind")

	// Timed out.
	slow, _ := h.svc.CreateOrder(ctx, "c-1", "key-5", []domain.Line{{SKU: "A", Quantity: 1}})
	*h.clock = h.clock.Add(time.Hour)
	if n, err := h.svc.ExpireStale(ctx, time.Minute, 10); err != nil || n != 1 {
		t.Fatalf("expired %d, %v; only the unfinished order should time out", n, err)
	}
	_ = slow

	if h.metrics.created != 5 {
		t.Errorf("created = %d, want 5", h.metrics.created)
	}
	want := []string{"confirmed", "cancelled/payment_failed", "cancelled/out_of_stock", "cancelled/customer", "cancelled/timeout"}
	if !reflect.DeepEqual(h.metrics.finished, want) {
		t.Fatalf("finished = %v\nwant     = %v", h.metrics.finished, want)
	}
}

func TestRepeatsAndDuplicatesAreNotCounted(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	_ = h.svc.HandlePaymentSucceeded(ctx, o.ID)

	// The same request again, and every reply again.
	_, _ = h.svc.CreateOrder(ctx, "c-1", "key-1", []domain.Line{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 4}})
	for range 3 {
		_ = h.svc.HandleStockReserved(ctx, o.ID)
		_ = h.svc.HandlePaymentSucceeded(ctx, o.ID)
		_ = h.svc.HandlePaymentFailed(ctx, o.ID, "late")
	}
	_, _ = h.svc.CancelOrder(ctx, o.ID, "too late") // refused: confirmed

	if h.metrics.created != 1 || len(h.metrics.finished) != 1 {
		t.Fatalf("created = %d, finished = %v; repeats must not be counted", h.metrics.created, h.metrics.finished)
	}

	// Cancelling an already cancelled order is a repeat as well.
	other, _ := h.svc.CreateOrder(ctx, "c-1", "key-2", []domain.Line{{SKU: "A", Quantity: 1}})
	_, _ = h.svc.CancelOrder(ctx, other.ID, "first")
	_, _ = h.svc.CancelOrder(ctx, other.ID, "second")
	if len(h.metrics.finished) != 2 {
		t.Fatalf("finished = %v, want the cancellation counted once", h.metrics.finished)
	}
}

func TestARefundFlagIsCountedOnce(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	_, _ = h.svc.CancelOrder(ctx, o.ID, "changed my mind")

	for range 3 {
		_ = h.svc.HandlePaymentSucceeded(ctx, o.ID)
	}
	if h.metrics.refunds != 1 {
		t.Fatalf("refunds = %d, want 1: money owed is counted once however often the payment event arrives", h.metrics.refunds)
	}
}

func TestMetricsAreNotRecordedForAStepThatRollsBack(t *testing.T) {
	h := setup()
	o := h.create(t)
	_ = h.svc.HandleStockReserved(ctx, o.ID)
	createdBefore := h.metrics.created

	h.repo.failOn = "save" // the transaction fails and is rolled back
	if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); err == nil {
		t.Fatal("expected the save to fail")
	}
	if _, err := h.svc.CancelOrder(ctx, o.ID, "x"); err == nil {
		t.Fatal("expected the save to fail")
	}
	*h.clock = h.clock.Add(time.Hour)
	if _, err := h.svc.ExpireStale(ctx, time.Minute, 10); err == nil {
		t.Fatal("expected the save to fail")
	}

	if len(h.metrics.finished) != 0 || h.metrics.refunds != 0 || h.metrics.created != createdBefore {
		t.Fatalf("finished = %v, refunds = %d: a rolled-back step must leave no trace in the metrics (the retry will count it)", h.metrics.finished, h.metrics.refunds)
	}

	// The retry after the database recovers is counted, once.
	h.repo.failOn = ""
	if err := h.svc.HandlePaymentSucceeded(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.metrics.finished) != 1 {
		t.Fatalf("finished = %v, want exactly one after the retry", h.metrics.finished)
	}
}

func TestGetOrder(t *testing.T) {
	h := setup()
	o := h.create(t)
	if got, err := h.svc.GetOrder(ctx, o.ID); err != nil || got.ID != o.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := h.svc.GetOrder(ctx, "nope"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err = %v", err)
	}
}
