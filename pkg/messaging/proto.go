package messaging

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// NewProtoMessage builds a Message carrying a protobuf payload. It records the
// payload's fully qualified type in HeaderMessageType and copies the
// correlation ID from ctx, so replies stay traceable to the request that
// started the flow. The ID is left empty for the outbox to assign.
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
	return Message{Subject: subject, Data: data, Headers: headers}, nil
}
