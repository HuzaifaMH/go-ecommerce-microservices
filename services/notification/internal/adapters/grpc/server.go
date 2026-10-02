// Package grpc is the inbound gRPC adapter of the notification service.
package grpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/pagination"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

// Notifications is the part of the application this adapter calls.
type Notifications interface {
	ListNotifications(ctx context.Context, orderID, customerID string, pageSize int, after *app.Cursor) ([]domain.Notification, *app.Cursor, error)
}

// Server implements notificationv1.NotificationServiceServer.
type Server struct {
	notificationv1.UnimplementedNotificationServiceServer
	notifications Notifications
	log           *slog.Logger
}

// NewServer returns the gRPC adapter.
func NewServer(n Notifications, log *slog.Logger) *Server {
	return &Server{notifications: n, log: log}
}

func (s *Server) ListNotifications(ctx context.Context, req *notificationv1.ListNotificationsRequest) (*notificationv1.ListNotificationsResponse, error) {
	pageSize, err := pagination.Size(req.GetPageSize())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	after, err := decodeCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}

	items, next, err := s.notifications.ListNotifications(ctx, req.GetOrderId(), req.GetCustomerId(), pageSize, after)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		return nil, status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		// Log the details, but do not leak them to the caller.
		s.log.Error("list notifications failed", "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	resp := &notificationv1.ListNotificationsResponse{Notifications: make([]*notificationv1.Notification, len(items))}
	for i, n := range items {
		resp.Notifications[i] = toProto(n)
	}
	if next != nil {
		resp.NextPageToken = pagination.Encode(next.CreatedAt, next.ID)
	}
	return resp, nil
}

func decodeCursor(token string) (*app.Cursor, error) {
	createdAt, id, ok, err := pagination.Decode(token)
	if err != nil || !ok {
		return nil, err
	}
	return &app.Cursor{CreatedAt: createdAt, ID: id}, nil
}

func toProto(n domain.Notification) *notificationv1.Notification {
	return &notificationv1.Notification{
		Id: n.ID, OrderId: n.OrderID, CustomerId: n.CustomerID,
		Kind: toProtoKind(n.Kind), Channel: toProtoChannel(n.Channel),
		Recipient: n.Recipient, Subject: n.Subject, Body: n.Body,
		Status: toProtoStatus(n.Status), FailureReason: n.FailureReason,
		CreateTime: timestamppb.New(n.CreatedAt), UpdateTime: timestamppb.New(n.UpdatedAt),
	}
}

func toProtoKind(k domain.Kind) notificationv1.NotificationKind {
	switch k {
	case domain.KindOrderConfirmed:
		return notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CONFIRMED
	case domain.KindOrderCancelled:
		return notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CANCELLED
	default:
		return notificationv1.NotificationKind_NOTIFICATION_KIND_UNSPECIFIED
	}
}

func toProtoChannel(c domain.Channel) notificationv1.NotificationChannel {
	switch c {
	case domain.ChannelEmail:
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL
	case domain.ChannelSMS:
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_SMS
	default:
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_UNSPECIFIED
	}
}

func toProtoStatus(s domain.Status) notificationv1.NotificationStatus {
	switch s {
	case domain.StatusPending:
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_PENDING
	case domain.StatusSent:
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_SENT
	case domain.StatusFailed:
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_FAILED
	default:
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_UNSPECIFIED
	}
}
