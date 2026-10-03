package messaging

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
)

// NewProtoMessage builds a Message carrying a protobuf payload. It records the
// payload's fully qualified type in HeaderMessageType, copies the correlation
// ID from ctx, and captures the current trace context (W3C traceparent), so
// replies stay traceable to the request that started the flow.
//
// The trace context is stored in the headers rather than read later because
// messages usually go through the outbox: they are published by another
// goroutine, long after the request that created them has finished. The ID is
// left empty for the outbox to assign.
func NewProtoMessage(ctx context.Context, subject string, payload proto.Message) (Message, error) {
	data, err := proto.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("marshal %s: %w", subject, err)
	}
	headers := map[string]string{
		HeaderMessageType: string(payload.ProtoReflect().Descriptor().FullName()),
	}
	if id := CorrelationID(ctx); id != "" {
		headers[HeaderCorrelationID] = id
	}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(headers))
	return Message{Subject: subject, Data: data, Headers: headers}, nil
}
