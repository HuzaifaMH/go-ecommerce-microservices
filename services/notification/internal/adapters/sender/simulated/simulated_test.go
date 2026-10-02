package simulated

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

func notification(id, recipient string, ch domain.Channel) domain.Notification {
	return domain.Notification{ID: id, OrderID: "o-1", Recipient: recipient, Channel: ch, Subject: "Hello"}
}

func newSender() (*Sender, *bytes.Buffer) {
	var buf bytes.Buffer
	return New(slog.New(slog.NewTextHandler(&buf, nil))), &buf
}

func TestNormalRecipientIsDeliveredAndLogged(t *testing.T) {
	s, buf := newSender()

	if err := s.Send(context.Background(), notification("n-1", "alice", domain.ChannelEmail)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"notification sent", "channel=email", "recipient=alice", "order_id=o-1", "notification_id=n-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

func TestBouncingRecipientIsUndeliverableWithAChannelSpecificReason(t *testing.T) {
	s, buf := newSender()

	for ch, want := range map[domain.Channel]string{
		domain.ChannelEmail: "mailbox does not exist",
		domain.ChannelSMS:   "number not reachable",
	} {
		err := s.Send(context.Background(), notification("n-1", "bounce-bob", ch))
		var u *domain.UndeliverableError
		if !errors.As(err, &u) || u.Reason != want {
			t.Errorf("%s: err = %v, want undeliverable %q", ch, err, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("nothing may be logged as sent: %q", buf.String())
	}
}

func TestFlakyRecipientFailsOncePerNotificationThenSucceeds(t *testing.T) {
	s, _ := newSender()
	ctx := context.Background()

	err := s.Send(ctx, notification("n-1", "flaky-carol", domain.ChannelEmail))
	var u *domain.UndeliverableError
	if err == nil || errors.As(err, &u) {
		t.Fatalf("first attempt must be a retryable failure, got %v", err)
	}
	if err := s.Send(ctx, notification("n-1", "flaky-carol", domain.ChannelEmail)); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	// Another notification is flaky again, independently.
	if err := s.Send(ctx, notification("n-2", "flaky-carol", domain.ChannelSMS)); err == nil {
		t.Fatal("the first attempt of another notification must fail too")
	}
}

func TestCancelledContextIsHonoured(t *testing.T) {
	s, _ := newSender()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, notification("n-1", "alice", domain.ChannelEmail)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
