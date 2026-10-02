package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/pagination"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

var created = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type fakeNotifications struct {
	items []domain.Notification
	next  *app.Cursor
	err   error

	gotOrder, gotCustomer string
	gotPage               int
	gotAfter              *app.Cursor
}

func (f *fakeNotifications) ListNotifications(_ context.Context, orderID, customerID string, size int, after *app.Cursor) ([]domain.Notification, *app.Cursor, error) {
	f.gotOrder, f.gotCustomer, f.gotPage, f.gotAfter = orderID, customerID, size, after
	return f.items, f.next, f.err
}

func client(t *testing.T, n Notifications) notificationv1.NotificationServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	notificationv1.RegisterNotificationServiceServer(srv, NewServer(n, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return notificationv1.NewNotificationServiceClient(conn)
}

func sample(id string, ch domain.Channel, st domain.Status) domain.Notification {
	return domain.Notification{
		ID: id, OrderID: "o-1", CustomerID: "alice", Kind: domain.KindOrderCancelled, Channel: ch,
		Recipient: "alice", Subject: "Your order was cancelled", Body: "body",
		Status: st, CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
}

func TestListNotificationsMapsAllFields(t *testing.T) {
	failed := sample("n-2", domain.ChannelSMS, domain.StatusFailed)
	failed.FailureReason = "number not reachable"
	f := &fakeNotifications{items: []domain.Notification{sample("n-1", domain.ChannelEmail, domain.StatusSent), failed}}

	resp, err := client(t, f).ListNotifications(context.Background(), &notificationv1.ListNotificationsRequest{OrderId: "o-1", CustomerId: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if f.gotOrder != "o-1" || f.gotCustomer != "alice" || f.gotPage != pagination.DefaultSize {
		t.Errorf("application received order=%q customer=%q page=%d", f.gotOrder, f.gotCustomer, f.gotPage)
	}

	if len(resp.GetNotifications()) != 2 {
		t.Fatalf("got %d notifications", len(resp.GetNotifications()))
	}
	n := resp.GetNotifications()[0]
	if n.GetId() != "n-1" || n.GetOrderId() != "o-1" || n.GetCustomerId() != "alice" || n.GetRecipient() != "alice" ||
		n.GetKind() != notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CANCELLED ||
		n.GetChannel() != notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL ||
		n.GetStatus() != notificationv1.NotificationStatus_NOTIFICATION_STATUS_SENT ||
		n.GetSubject() != "Your order was cancelled" || n.GetBody() != "body" ||
		!n.GetCreateTime().AsTime().Equal(created) || !n.GetUpdateTime().AsTime().Equal(created.Add(time.Minute)) {
		t.Fatalf("notification = %v", n)
	}
	sms := resp.GetNotifications()[1]
	if sms.GetChannel() != notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_SMS ||
		sms.GetStatus() != notificationv1.NotificationStatus_NOTIFICATION_STATUS_FAILED || sms.GetFailureReason() != "number not reachable" {
		t.Fatalf("sms = %v", sms)
	}
}

func TestEnumMapping(t *testing.T) {
	if toProtoKind("other") != notificationv1.NotificationKind_NOTIFICATION_KIND_UNSPECIFIED ||
		toProtoChannel("other") != notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_UNSPECIFIED ||
		toProtoStatus("other") != notificationv1.NotificationStatus_NOTIFICATION_STATUS_UNSPECIFIED {
		t.Error("unknown values must map to UNSPECIFIED")
	}
	if toProtoKind(domain.KindOrderConfirmed) != notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CONFIRMED ||
		toProtoStatus(domain.StatusPending) != notificationv1.NotificationStatus_NOTIFICATION_STATUS_PENDING {
		t.Error("known values must map to their counterparts")
	}
}

func TestPaginationRoundTrip(t *testing.T) {
	f := &fakeNotifications{
		items: []domain.Notification{sample("n-2", domain.ChannelSMS, domain.StatusSent)},
		next:  &app.Cursor{CreatedAt: created, ID: "n-2"},
	}
	c := client(t, f)

	first, err := c.ListNotifications(context.Background(), &notificationv1.ListNotificationsRequest{PageSize: 1})
	if err != nil || first.GetNextPageToken() == "" || f.gotPage != 1 || f.gotAfter != nil {
		t.Fatalf("first = %v, err = %v, page = %d, after = %v", first, err, f.gotPage, f.gotAfter)
	}

	f.next = nil
	last, err := c.ListNotifications(context.Background(), &notificationv1.ListNotificationsRequest{PageSize: 1, PageToken: first.GetNextPageToken()})
	if err != nil || last.GetNextPageToken() != "" {
		t.Fatalf("last = %v, err = %v", last, err)
	}
	if f.gotAfter == nil || f.gotAfter.ID != "n-2" || !f.gotAfter.CreatedAt.Equal(created) {
		t.Fatalf("cursor = %+v, want the one from the first page", f.gotAfter)
	}
}

func TestRequestValidation(t *testing.T) {
	c := client(t, &fakeNotifications{})
	ctx := context.Background()

	if _, err := c.ListNotifications(ctx, &notificationv1.ListNotificationsRequest{PageSize: -1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative page size: %v", err)
	}
	if _, err := c.ListNotifications(ctx, &notificationv1.ListNotificationsRequest{PageToken: "!!!not-base64!!!"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad token: %v", err)
	}
}

func TestPageSizeIsCapped(t *testing.T) {
	f := &fakeNotifications{}
	_, _ = client(t, f).ListNotifications(context.Background(), &notificationv1.ListNotificationsRequest{PageSize: 100000})
	if f.gotPage != pagination.MaxSize {
		t.Errorf("page size = %d, want it capped at %d", f.gotPage, pagination.MaxSize)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"canceled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"unexpected", errors.New("pq: password authentication failed"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client(t, &fakeNotifications{err: tc.err}).ListNotifications(context.Background(), &notificationv1.ListNotificationsRequest{})
			st, _ := status.FromError(err)
			if st.Code() != tc.want {
				t.Fatalf("code = %v (%v), want %v", st.Code(), err, tc.want)
			}
			if tc.want == codes.Internal && st.Message() != "internal error" {
				t.Errorf("internal details leaked: %q", st.Message())
			}
		})
	}
}
