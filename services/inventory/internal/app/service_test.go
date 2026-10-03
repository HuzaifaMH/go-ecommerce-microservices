package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

// fakeRepo is an in-memory Repository. Its Transactor snapshots state so a
// failed transaction rolls back, like a real database.
type fakeRepo struct {
	items        map[string]domain.Item
	reservations map[string]domain.Reservation
	failSave     error
}

func newFakeRepo(items ...domain.Item) *fakeRepo {
	r := &fakeRepo{items: map[string]domain.Item{}, reservations: map[string]domain.Reservation{}}
	for _, it := range items {
		r.items[it.SKU] = it
	}
	return r
}

func (r *fakeRepo) InTx(ctx context.Context, fn func(context.Context) error) error {
	items := make(map[string]domain.Item, len(r.items))
	for k, v := range r.items {
		items[k] = v
	}
	reservations := make(map[string]domain.Reservation, len(r.reservations))
	for k, v := range r.reservations {
		reservations[k] = v
	}
	if err := fn(ctx); err != nil {
		r.items, r.reservations = items, reservations
		return err
	}
	return nil
}

func (r *fakeRepo) GetItem(_ context.Context, sku string) (domain.Item, error) {
	it, ok := r.items[sku]
	if !ok {
		return domain.Item{}, domain.ErrItemNotFound
	}
	return it, nil
}

func (r *fakeRepo) ListItems(_ context.Context, limit int, after string) ([]domain.Item, error) {
	var skus []string
	for sku := range r.items {
		if sku > after {
			skus = append(skus, sku)
		}
	}
	sort.Strings(skus)
	if len(skus) > limit {
		skus = skus[:limit]
	}
	out := make([]domain.Item, len(skus))
	for i, s := range skus {
		out[i] = r.items[s]
	}
	return out, nil
}

func (r *fakeRepo) LockItems(_ context.Context, skus []string) (map[string]domain.Item, error) {
	out := map[string]domain.Item{}
	for _, s := range skus {
		if it, ok := r.items[s]; ok {
			out[s] = it
		}
	}
	return out, nil
}

func (r *fakeRepo) SaveItems(_ context.Context, items map[string]domain.Item) error {
	if r.failSave != nil {
		return r.failSave
	}
	for k, v := range items {
		r.items[k] = v
	}
	return nil
}

func (r *fakeRepo) FindReservation(_ context.Context, orderID string) (domain.Reservation, error) {
	res, ok := r.reservations[orderID]
	if !ok {
		return domain.Reservation{}, domain.ErrReservationNotFound
	}
	return res, nil
}

func (r *fakeRepo) CreateReservation(_ context.Context, res domain.Reservation) error {
	r.reservations[res.OrderID] = res
	return nil
}

func (r *fakeRepo) SetReservationStatus(_ context.Context, orderID string, st domain.ReservationStatus) error {
	res := r.reservations[orderID]
	res.Status = st
	r.reservations[orderID] = res
	return nil
}

type recordedEvents struct{ log []string }

func (e *recordedEvents) StockReserved(_ context.Context, id string) error {
	e.log = append(e.log, "reserved:"+id)
	return nil
}

func (e *recordedEvents) StockRejected(_ context.Context, id, reason string, skus []string) error {
	e.log = append(e.log, fmt.Sprintf("rejected:%s:%s:%v", id, reason, skus))
	return nil
}

func (e *recordedEvents) StockReleased(_ context.Context, id string) error {
	e.log = append(e.log, "released:"+id)
	return nil
}

func setup(items ...domain.Item) (*Service, *fakeRepo, *recordedEvents) {
	repo := newFakeRepo(items...)
	ev := &recordedEvents{}
	return NewService(repo, repo, ev), repo, ev
}

func TestReserveSuccess(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10}, domain.Item{SKU: "B", OnHand: 5})

	err := svc.Reserve(context.Background(), "o-1", []domain.Line{{SKU: "A", Quantity: 3}, {SKU: "B", Quantity: 5}})
	if err != nil {
		t.Fatal(err)
	}

	if repo.items["A"].Reserved != 3 || repo.items["B"].Reserved != 5 {
		t.Errorf("stock not reserved: %+v", repo.items)
	}
	if repo.reservations["o-1"].Status != domain.ReservationActive {
		t.Errorf("reservation = %+v", repo.reservations["o-1"])
	}
	if !reflect.DeepEqual(ev.log, []string{"reserved:o-1"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestReserveRejectsWholeOrderWhenAnyLineIsShort(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10}, domain.Item{SKU: "B", OnHand: 1})

	err := svc.Reserve(context.Background(), "o-1", []domain.Line{{SKU: "A", Quantity: 3}, {SKU: "B", Quantity: 2}, {SKU: "Z", Quantity: 1}})
	if err != nil {
		t.Fatalf("a rejection is a business outcome, not an error: %v", err)
	}

	if repo.items["A"].Reserved != 0 || repo.items["B"].Reserved != 0 {
		t.Errorf("nothing may be reserved on rejection: %+v", repo.items)
	}
	if len(repo.reservations) != 0 {
		t.Errorf("no reservation expected: %+v", repo.reservations)
	}
	want := []string{"rejected:o-1:insufficient stock:[B Z]"}
	if !reflect.DeepEqual(ev.log, want) {
		t.Errorf("events = %v, want %v", ev.log, want)
	}
}

func TestReserveIsIdempotentPerOrder(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10})
	lines := []domain.Line{{SKU: "A", Quantity: 4}}

	for range 3 {
		if err := svc.Reserve(context.Background(), "o-1", lines); err != nil {
			t.Fatal(err)
		}
	}
	if repo.items["A"].Reserved != 4 {
		t.Fatalf("Reserved = %d, want 4 (must not accumulate)", repo.items["A"].Reserved)
	}
	if len(ev.log) != 3 || ev.log[2] != "reserved:o-1" {
		t.Errorf("each attempt should re-publish the outcome: %v", ev.log)
	}
}

func TestReserveAfterReleaseIsRejected(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10})
	ctx := context.Background()
	_ = svc.Reserve(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 4}})
	_ = svc.Release(ctx, "o-1")
	ev.log = nil

	if err := svc.Reserve(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 4}}); err != nil {
		t.Fatal(err)
	}
	if repo.items["A"].Reserved != 0 {
		t.Errorf("a released order must not re-reserve stock: %+v", repo.items["A"])
	}
	if len(ev.log) != 1 || ev.log[0] != "rejected:o-1:reservation was already released:[]" {
		t.Errorf("events = %v", ev.log)
	}
}

func TestReserveInvalidCommand(t *testing.T) {
	svc, _, ev := setup(domain.Item{SKU: "A", OnHand: 10})

	err := svc.Reserve(context.Background(), "o-1", nil)
	if !errors.Is(err, domain.ErrInvalidReservation) {
		t.Fatalf("err = %v, want ErrInvalidReservation", err)
	}
	if len(ev.log) != 0 {
		t.Errorf("no event expected: %v", ev.log)
	}
}

func TestReserveRollsBackOnPersistenceFailure(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10})
	repo.failSave = errors.New("disk full")

	if err := svc.Reserve(context.Background(), "o-1", []domain.Line{{SKU: "A", Quantity: 1}}); err == nil {
		t.Fatal("expected error")
	}
	if len(repo.reservations) != 0 || len(ev.log) != 0 {
		t.Errorf("state leaked after failure: %+v %v", repo.reservations, ev.log)
	}
}

func TestReleaseRestoresStockAndIsIdempotent(t *testing.T) {
	svc, repo, ev := setup(domain.Item{SKU: "A", OnHand: 10})
	ctx := context.Background()
	if err := svc.Reserve(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 4}}); err != nil {
		t.Fatal(err)
	}
	ev.log = nil

	for range 2 {
		if err := svc.Release(ctx, "o-1"); err != nil {
			t.Fatal(err)
		}
	}
	if repo.items["A"].Reserved != 0 {
		t.Errorf("Reserved = %d, want 0", repo.items["A"].Reserved)
	}
	if repo.reservations["o-1"].Status != domain.ReservationReleased {
		t.Errorf("status = %q", repo.reservations["o-1"].Status)
	}
	if !reflect.DeepEqual(ev.log, []string{"released:o-1", "released:o-1"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestReleaseUnknownOrderStillConfirms(t *testing.T) {
	svc, _, ev := setup()
	if err := svc.Release(context.Background(), "never-reserved"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev.log, []string{"released:never-reserved"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestReleaseRequiresOrderID(t *testing.T) {
	svc, _, _ := setup()
	if err := svc.Release(context.Background(), ""); !errors.Is(err, domain.ErrInvalidReservation) {
		t.Fatalf("err = %v", err)
	}
}

type fakeMetrics struct{ reservations, releases []string }

func (m *fakeMetrics) Reservation(outcome string) { m.reservations = append(m.reservations, outcome) }
func (m *fakeMetrics) Release(outcome string)     { m.releases = append(m.releases, outcome) }

func TestMetricsReportEachOutcomeOnceAndOnlyAfterCommit(t *testing.T) {
	svc, repo, _ := setup(domain.Item{SKU: "A", OnHand: 5})
	m := &fakeMetrics{}
	svc.WithMetrics(m)
	ctx := context.Background()

	_ = svc.Reserve(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 3}}) // reserved
	_ = svc.Reserve(ctx, "o-1", []domain.Line{{SKU: "A", Quantity: 3}}) // same order again: duplicate
	_ = svc.Reserve(ctx, "o-2", []domain.Line{{SKU: "A", Quantity: 3}}) // only 2 left: rejected
	_ = svc.Release(ctx, "o-1")                                         // released
	_ = svc.Release(ctx, "o-1")                                         // already released: noop
	_ = svc.Release(ctx, "never-reserved")                              // noop
	_ = svc.Reserve(ctx, "o-1", nil)                                    // malformed: not a business outcome

	if want := []string{"reserved", "duplicate", "rejected"}; !reflect.DeepEqual(m.reservations, want) {
		t.Errorf("reservations = %v, want %v", m.reservations, want)
	}
	if want := []string{"released", "noop", "noop"}; !reflect.DeepEqual(m.releases, want) {
		t.Errorf("releases = %v, want %v", m.releases, want)
	}

	// A failure that rolls the transaction back counts nothing.
	m.reservations = nil
	repo.failSave = errors.New("disk full")
	if err := svc.Reserve(ctx, "o-3", []domain.Line{{SKU: "A", Quantity: 1}}); err == nil {
		t.Fatal("expected error")
	}
	if len(m.reservations) != 0 {
		t.Errorf("reservations = %v after a rolled-back reserve, want none", m.reservations)
	}
}

func TestListItemsPagination(t *testing.T) {
	svc, _, _ := setup(
		domain.Item{SKU: "A"}, domain.Item{SKU: "B"}, domain.Item{SKU: "C"}, domain.Item{SKU: "D"}, domain.Item{SKU: "E"})
	ctx := context.Background()

	page1, next, err := svc.ListItems(ctx, 2, "")
	if err != nil || len(page1) != 2 || next != "B" {
		t.Fatalf("page1 = %v next=%q err=%v", page1, next, err)
	}
	page2, next, _ := svc.ListItems(ctx, 2, next)
	if len(page2) != 2 || page2[0].SKU != "C" || next != "D" {
		t.Fatalf("page2 = %v next=%q", page2, next)
	}
	page3, next, _ := svc.ListItems(ctx, 2, next)
	if len(page3) != 1 || page3[0].SKU != "E" || next != "" {
		t.Fatalf("page3 = %v next=%q (last page has no next token)", page3, next)
	}
}
