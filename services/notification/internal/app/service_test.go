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

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

// fakeRepo is an in-memory Repository keyed like the real one.
type fakeRepo struct {
	byKey      map[string]*domain.Notification
	order      []string // keys in creation order
	failEnsure error
	failMark   error
}

func newFakeRepo() *fakeRepo { return &fakeRepo{byKey: map[string]*domain.Notification{}} }

func key(n domain.Notification) string { return fmt.Sprintf("%s/%s/%s", n.OrderID, n.Kind, n.Channel) }

func (r *fakeRepo) Ensure(_ context.Context, ns []domain.Notification) ([]domain.Notification, error) {
	if r.failEnsure != nil {
		return nil, r.failEnsure
	}
	out := make([]domain.Notification, len(ns))
	for i, n := range ns {
		k := key(n)
		if existing, ok := r.byKey[k]; ok {
			out[i] = *existing
			continue
		}
		cp := n
		r.byKey[k] = &cp
		r.order = append(r.order, k)
		out[i] = cp
	}
	return out, nil
}

func (r *fakeRepo) find(id string) *domain.Notification {
	for _, n := range r.byKey {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (r *fakeRepo) MarkSent(_ context.Context, id string, at time.Time) error {
	if r.failMark != nil {
		return r.failMark
	}
	n := r.find(id)
	n.Status, n.UpdatedAt = domain.StatusSent, at
	return nil
}

func (r *fakeRepo) MarkFailed(_ context.Context, id, reason string, at time.Time) error {
	n := r.find(id)
	n.Status, n.FailureReason, n.UpdatedAt = domain.StatusFailed, reason, at
	return nil
}

func (r *fakeRepo) List(_ context.Context, q ListQuery) ([]domain.Notification, error) {
	var all []domain.Notification
	for _, n := range r.byKey {
		if (q.OrderID == "" || n.OrderID == q.OrderID) && (q.CustomerID == "" || n.CustomerID == q.CustomerID) {
			all = append(all, *n)
		}
	}
	sort.Slice(all, func(a, b int) bool {
		if !all[a].CreatedAt.Equal(all[b].CreatedAt) {
			return all[a].CreatedAt.After(all[b].CreatedAt)
		}
		return all[a].ID > all[b].ID
	})
	if q.After != nil {
		var rest []domain.Notification
		for _, n := range all {
			if n.CreatedAt.Before(q.After.CreatedAt) || (n.CreatedAt.Equal(q.After.CreatedAt) && n.ID < q.After.ID) {
				rest = append(rest, n)
			}
		}
		all = rest
	}
	if len(all) > q.Limit {
		all = all[:q.Limit]
	}
	return all, nil
}

// fakeSender records what it delivered and can be scripted per channel.
type fakeSender struct {
	sent      []string               // "<channel>:<order>"
	transient map[domain.Channel]int // channel -> remaining transient failures
	permanent map[domain.Channel]bool
	attempts  int
}

func (s *fakeSender) Send(_ context.Context, n domain.Notification) error {
	s.attempts++
	if s.permanent[n.Channel] {
		return &domain.UndeliverableError{Reason: "no such address"}
	}
	if s.transient[n.Channel] > 0 {
		s.transient[n.Channel]--
		return errors.New("gateway timeout")
	}
	s.sent = append(s.sent, fmt.Sprintf("%s:%s", n.Channel, n.OrderID))
	return nil
}

type harness struct {
	svc    *Service
	repo   *fakeRepo
	sender *fakeSender
}

func setup() *harness {
	repo := newFakeRepo()
	sender := &fakeSender{transient: map[domain.Channel]int{}, permanent: map[domain.Channel]bool{}}
	n := 0
	clock := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	svc := NewService(repo, sender, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() string { n++; return fmt.Sprintf("n-%d", n) },
		func() time.Time { clock = clock.Add(time.Second); return clock })
	return &harness{svc: svc, repo: repo, sender: sender}
}

var ctx = context.Background()
var usd = domain.Money{CurrencyCode: "USD", AmountMinor: 3000}

func (h *harness) status(order string, kind domain.Kind, ch domain.Channel) domain.Status {
	return h.repo.byKey[fmt.Sprintf("%s/%s/%s", order, kind, ch)].Status
}

func TestConfirmedOrderSendsOneEmail(t *testing.T) {
	h := setup()

	if err := h.svc.NotifyOrderConfirmed(ctx, "o-1", "alice", usd); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.sender.sent, []string{"email:o-1"}) {
		t.Fatalf("sent = %v", h.sender.sent)
	}
	if h.status("o-1", domain.KindOrderConfirmed, domain.ChannelEmail) != domain.StatusSent {
		t.Error("the notification should be marked sent")
	}
}

func TestCancelledOrderSendsEmailAndSMS(t *testing.T) {
	h := setup()

	if err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "payment failed: card declined"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.sender.sent, []string{"email:o-1", "sms:o-1"}) {
		t.Fatalf("sent = %v", h.sender.sent)
	}
}

func TestRepeatedEventNeverNotifiesTwice(t *testing.T) {
	h := setup()

	for range 3 {
		if err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout"); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.NotifyOrderConfirmed(ctx, "o-2", "alice", usd); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"email:o-1", "sms:o-1", "email:o-2"}; !reflect.DeepEqual(h.sender.sent, want) {
		t.Fatalf("sent = %v, want %v (each exactly once)", h.sender.sent, want)
	}
	if len(h.repo.byKey) != 3 {
		t.Errorf("%d notifications recorded, want 3", len(h.repo.byKey))
	}
}

func TestTransientFailureIsRetriedAndOnlyThePendingOneIsResent(t *testing.T) {
	h := setup()
	h.sender.transient[domain.ChannelSMS] = 1 // SMS gateway fails once

	err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout")
	if err == nil {
		t.Fatal("a transient failure must be returned so the event is redelivered")
	}
	if h.status("o-1", domain.KindOrderCancelled, domain.ChannelEmail) != domain.StatusSent {
		t.Error("the email must not be held back by the failing SMS")
	}
	if h.status("o-1", domain.KindOrderCancelled, domain.ChannelSMS) != domain.StatusPending {
		t.Error("the SMS must stay pending for the retry")
	}

	// The broker redelivers the event.
	if err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"email:o-1", "sms:o-1"}; !reflect.DeepEqual(h.sender.sent, want) {
		t.Fatalf("sent = %v, want %v: the email must not be sent a second time", h.sender.sent, want)
	}
	if h.status("o-1", domain.KindOrderCancelled, domain.ChannelSMS) != domain.StatusSent {
		t.Error("the SMS should now be sent")
	}
}

func TestUndeliverableIsRecordedAsFailedAndNotRetried(t *testing.T) {
	h := setup()
	h.sender.permanent[domain.ChannelSMS] = true

	if err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout"); err != nil {
		t.Fatalf("an undeliverable message is a result, not an error: %v", err)
	}
	sms := h.repo.byKey["o-1/order_cancelled/sms"]
	if sms.Status != domain.StatusFailed || sms.FailureReason != "no such address" {
		t.Fatalf("sms = %+v", sms)
	}
	if h.status("o-1", domain.KindOrderCancelled, domain.ChannelEmail) != domain.StatusSent {
		t.Error("the email should still be sent")
	}

	attempts := h.sender.attempts
	if err := h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout"); err != nil {
		t.Fatal(err)
	}
	if h.sender.attempts != attempts {
		t.Errorf("a failed or sent notification was attempted again (%d -> %d attempts)", attempts, h.sender.attempts)
	}
}

type fakeMetrics struct{ deliveries []string }

func (m *fakeMetrics) Delivery(ch domain.Channel, outcome string) {
	m.deliveries = append(m.deliveries, string(ch)+"/"+outcome)
}

func TestMetricsCountEveryDeliveryAttemptByOutcome(t *testing.T) {
	h := setup()
	m := &fakeMetrics{}
	h.svc.WithMetrics(m)

	// SMS fails once (transient), then the event is redelivered.
	h.sender.transient[domain.ChannelSMS] = 1
	_ = h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout")
	_ = h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout")
	// A repeat of a fully delivered event attempts nothing.
	_ = h.svc.NotifyOrderCancelled(ctx, "o-1", "alice", "saga timeout")

	// Another order whose SMS is permanently undeliverable.
	h.sender.permanent[domain.ChannelSMS] = true
	_ = h.svc.NotifyOrderCancelled(ctx, "o-2", "bob", "saga timeout")

	want := []string{"email/sent", "sms/retry", "sms/sent", "email/sent", "sms/failed"}
	if !reflect.DeepEqual(m.deliveries, want) {
		t.Fatalf("deliveries = %v\nwant       = %v (a repeated event must not add attempts)", m.deliveries, want)
	}
}

func TestInvalidEventsAreRejectedBeforeAnythingIsRecorded(t *testing.T) {
	h := setup()

	if err := h.svc.NotifyOrderConfirmed(ctx, "o-1", "", usd); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Errorf("confirmed without customer: %v", err)
	}
	if err := h.svc.NotifyOrderCancelled(ctx, "", "alice", "x"); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Errorf("cancelled without order: %v", err)
	}
	if len(h.repo.byKey) != 0 || h.sender.attempts != 0 {
		t.Errorf("nothing may be recorded or sent: %d recorded, %d attempts", len(h.repo.byKey), h.sender.attempts)
	}
}

func TestStorageFailuresAreReturnedForRetry(t *testing.T) {
	h := setup()

	h.repo.failEnsure = errors.New("database down")
	if err := h.svc.NotifyOrderConfirmed(ctx, "o-1", "alice", usd); err == nil {
		t.Fatal("recording failed: expected an error")
	}
	if h.sender.attempts != 0 {
		t.Error("nothing may be sent if it could not be recorded first")
	}

	h.repo.failEnsure, h.repo.failMark = nil, errors.New("database down")
	if err := h.svc.NotifyOrderConfirmed(ctx, "o-2", "alice", usd); err == nil {
		t.Fatal("marking sent failed: expected an error so the event is redelivered")
	}
}

func TestListNotificationsFiltersAndPaginates(t *testing.T) {
	h := setup()
	_ = h.svc.NotifyOrderConfirmed(ctx, "o-1", "alice", usd)
	_ = h.svc.NotifyOrderCancelled(ctx, "o-2", "alice", "saga timeout") // two notifications
	_ = h.svc.NotifyOrderConfirmed(ctx, "o-3", "bob", usd)

	byOrder, next, err := h.svc.ListNotifications(ctx, "o-2", "", 10, nil)
	if err != nil || len(byOrder) != 2 || next != nil {
		t.Fatalf("by order: %d, next %v, err %v", len(byOrder), next, err)
	}
	byCustomer, _, _ := h.svc.ListNotifications(ctx, "", "alice", 10, nil)
	if len(byCustomer) != 3 {
		t.Fatalf("by customer: %d, want 3", len(byCustomer))
	}

	var got []string
	var after *Cursor
	for range 10 {
		page, cursor, err := h.svc.ListNotifications(ctx, "", "", 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page {
			got = append(got, n.ID)
		}
		if cursor == nil {
			break
		}
		after = cursor
	}
	if len(got) != 4 {
		t.Fatalf("paged %d notifications, want 4: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("notification %s appeared on two pages: %v", id, got)
		}
		seen[id] = true
	}
}
