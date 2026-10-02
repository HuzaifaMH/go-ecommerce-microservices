package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

// fakeRepo is an in-memory Repository whose Create keeps the first payment per order.
type fakeRepo struct {
	payments    map[string]domain.Payment
	failCreate  error
	beforeWrite func() // runs inside Create, to simulate a concurrent attempt
}

func newFakeRepo() *fakeRepo { return &fakeRepo{payments: map[string]domain.Payment{}} }

func (r *fakeRepo) FindByOrder(_ context.Context, orderID string) (domain.Payment, error) {
	p, ok := r.payments[orderID]
	if !ok {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	return p, nil
}

func (r *fakeRepo) Create(_ context.Context, p domain.Payment) (domain.Payment, error) {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	if r.failCreate != nil {
		return domain.Payment{}, r.failCreate
	}
	if existing, ok := r.payments[p.OrderID]; ok {
		return existing, nil
	}
	r.payments[p.OrderID] = p
	return p, nil
}

// InTx snapshots state so a failed transaction rolls back, like a real database.
func (r *fakeRepo) InTx(ctx context.Context, fn func(context.Context) error) error {
	snapshot := make(map[string]domain.Payment, len(r.payments))
	for k, v := range r.payments {
		snapshot[k] = v
	}
	if err := fn(ctx); err != nil {
		r.payments = snapshot
		return err
	}
	return nil
}

type fakeProvider struct {
	calls  int
	ref    string
	err    error
	result func(domain.ChargeRequest) (string, error)
}

func (p *fakeProvider) Charge(_ context.Context, req domain.ChargeRequest) (string, error) {
	p.calls++
	if p.result != nil {
		return p.result(req)
	}
	return p.ref, p.err
}

type recordedEvents struct{ log []string }

func (e *recordedEvents) PaymentSucceeded(_ context.Context, orderID, paymentID string) error {
	e.log = append(e.log, fmt.Sprintf("succeeded:%s:%s", orderID, paymentID))
	return nil
}

func (e *recordedEvents) PaymentFailed(_ context.Context, orderID, reason string) error {
	e.log = append(e.log, fmt.Sprintf("failed:%s:%s", orderID, reason))
	return nil
}

func setup(p *fakeProvider) (*Service, *fakeRepo, *recordedEvents) {
	repo := newFakeRepo()
	ev := &recordedEvents{}
	n := 0
	newID := func() string { n++; return fmt.Sprintf("pay-%d", n) }
	return NewService(p, repo, repo, ev, newID), repo, ev
}

var usd = func(n int64) domain.Money { return domain.Money{CurrencyCode: "USD", AmountMinor: n} }

func TestChargeSuccess(t *testing.T) {
	prov := &fakeProvider{ref: "ref-1"}
	svc, repo, ev := setup(prov)

	if err := svc.Charge(context.Background(), "o-1", "c-1", usd(1999)); err != nil {
		t.Fatal(err)
	}

	p := repo.payments["o-1"]
	if p.Status != domain.StatusSucceeded || p.ProviderRef != "ref-1" || p.Amount != usd(1999) {
		t.Errorf("payment = %+v", p)
	}
	if !reflect.DeepEqual(ev.log, []string{"succeeded:o-1:pay-1"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestDeclineIsRecordedAndReportedNotReturnedAsError(t *testing.T) {
	prov := &fakeProvider{err: &domain.DeclinedError{Reason: "insufficient funds"}}
	svc, repo, ev := setup(prov)

	if err := svc.Charge(context.Background(), "o-1", "c-1", usd(500)); err != nil {
		t.Fatalf("a decline is a business outcome, not an error: %v", err)
	}

	if p := repo.payments["o-1"]; p.Status != domain.StatusFailed || p.FailureReason != "insufficient funds" {
		t.Errorf("payment = %+v", p)
	}
	if !reflect.DeepEqual(ev.log, []string{"failed:o-1:insufficient funds"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestProviderOutageIsRetryableAndLeavesNoTrace(t *testing.T) {
	prov := &fakeProvider{err: errors.New("provider timeout")}
	svc, repo, ev := setup(prov)

	err := svc.Charge(context.Background(), "o-1", "c-1", usd(500))
	if err == nil {
		t.Fatal("expected error so the command is redelivered")
	}
	var declined *domain.DeclinedError
	if errors.As(err, &declined) || errors.Is(err, domain.ErrInvalidPayment) {
		t.Errorf("an outage must not look like a decline or a bad command: %v", err)
	}
	if len(repo.payments) != 0 || len(ev.log) != 0 {
		t.Errorf("nothing may be stored or published: %+v %v", repo.payments, ev.log)
	}
}

func TestChargeIsIdempotentAndDoesNotCallProviderAgain(t *testing.T) {
	prov := &fakeProvider{ref: "ref-1"}
	svc, _, ev := setup(prov)

	for range 3 {
		if err := svc.Charge(context.Background(), "o-1", "c-1", usd(1999)); err != nil {
			t.Fatal(err)
		}
	}
	if prov.calls != 1 {
		t.Fatalf("provider called %d times, want 1", prov.calls)
	}
	want := []string{"succeeded:o-1:pay-1", "succeeded:o-1:pay-1", "succeeded:o-1:pay-1"}
	if !reflect.DeepEqual(ev.log, want) {
		t.Errorf("each attempt must re-publish the original outcome: %v", ev.log)
	}
}

func TestRepeatedDeclineStaysDeclined(t *testing.T) {
	prov := &fakeProvider{err: &domain.DeclinedError{Reason: "blocked"}}
	svc, _, ev := setup(prov)

	_ = svc.Charge(context.Background(), "o-1", "c-1", usd(500))
	prov.err = nil // even if the provider would accept now, the recorded outcome stands
	_ = svc.Charge(context.Background(), "o-1", "c-1", usd(500))

	if prov.calls != 1 {
		t.Errorf("provider called %d times, want 1", prov.calls)
	}
	if !reflect.DeepEqual(ev.log, []string{"failed:o-1:blocked", "failed:o-1:blocked"}) {
		t.Errorf("events = %v", ev.log)
	}
}

func TestConflictingRepeatIsRejectedAsInvalid(t *testing.T) {
	svc, _, ev := setup(&fakeProvider{ref: "r"})
	_ = svc.Charge(context.Background(), "o-1", "c-1", usd(1999))
	ev.log = nil

	for name, args := range map[string]struct {
		customer string
		amount   domain.Money
	}{
		"different amount":   {"c-1", usd(2000)},
		"different customer": {"c-2", usd(1999)},
	} {
		err := svc.Charge(context.Background(), "o-1", args.customer, args.amount)
		if !errors.Is(err, domain.ErrInvalidPayment) {
			t.Errorf("%s: err = %v, want ErrInvalidPayment", name, err)
		}
	}
	if len(ev.log) != 0 {
		t.Errorf("no event expected: %v", ev.log)
	}
}

func TestConcurrentAttemptWinsAndItsOutcomeIsPublished(t *testing.T) {
	prov := &fakeProvider{ref: "mine"}
	svc, repo, ev := setup(prov)

	// Another worker stores a payment between our provider call and our write.
	repo.beforeWrite = func() {
		repo.beforeWrite = nil
		repo.payments["o-1"] = domain.Payment{ID: "pay-other", OrderID: "o-1", CustomerID: "c-1",
			Amount: usd(1999), Status: domain.StatusSucceeded, ProviderRef: "theirs"}
	}

	if err := svc.Charge(context.Background(), "o-1", "c-1", usd(1999)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev.log, []string{"succeeded:o-1:pay-other"}) {
		t.Errorf("must publish the stored (first) payment, got %v", ev.log)
	}
	if repo.payments["o-1"].ID != "pay-other" {
		t.Errorf("the first payment must be kept: %+v", repo.payments["o-1"])
	}
}

func TestChargeRollsBackWhenStoringFails(t *testing.T) {
	svc, repo, ev := setup(&fakeProvider{ref: "r"})
	repo.failCreate = errors.New("disk full")

	if err := svc.Charge(context.Background(), "o-1", "c-1", usd(100)); err == nil {
		t.Fatal("expected error")
	}
	if len(repo.payments) != 0 || len(ev.log) != 0 {
		t.Errorf("state leaked after failure: %+v %v", repo.payments, ev.log)
	}
}

func TestChargeRejectsInvalidCommandsBeforeAnyWork(t *testing.T) {
	prov := &fakeProvider{ref: "r"}
	svc, _, ev := setup(prov)

	err := svc.Charge(context.Background(), "", "c-1", usd(100))
	if !errors.Is(err, domain.ErrInvalidPayment) {
		t.Fatalf("err = %v", err)
	}
	if prov.calls != 0 || len(ev.log) != 0 {
		t.Errorf("no provider call or event expected (calls=%d events=%v)", prov.calls, ev.log)
	}
}

func TestGetPayment(t *testing.T) {
	svc, _, _ := setup(&fakeProvider{ref: "r"})
	ctx := context.Background()

	if _, err := svc.GetPayment(ctx, "o-1"); !errors.Is(err, domain.ErrPaymentNotFound) {
		t.Fatalf("err = %v, want ErrPaymentNotFound", err)
	}
	_ = svc.Charge(ctx, "o-1", "c-1", usd(100))
	if p, err := svc.GetPayment(ctx, "o-1"); err != nil || p.ID != "pay-1" {
		t.Fatalf("payment = %+v, err = %v", p, err)
	}
}
