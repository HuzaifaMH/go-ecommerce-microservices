package messaging

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestNewProtoMessage(t *testing.T) {
	ctx := WithCorrelationID(context.Background(), "corr-1")

	m, err := NewProtoMessage(ctx, "order.evt.confirmed", wrapperspb.String("hello"))
	if err != nil {
		t.Fatal(err)
	}

	if m.Subject != "order.evt.confirmed" || m.ID != "" {
		t.Errorf("subject/ID = %q/%q", m.Subject, m.ID)
	}
	if m.Headers[HeaderMessageType] != "google.protobuf.StringValue" {
		t.Errorf("message type = %q", m.Headers[HeaderMessageType])
	}
	if m.Headers[HeaderCorrelationID] != "corr-1" {
		t.Errorf("correlation ID = %q", m.Headers[HeaderCorrelationID])
	}
	var got wrapperspb.StringValue
	if err := proto.Unmarshal(m.Data, &got); err != nil || got.GetValue() != "hello" {
		t.Errorf("payload = %v, %v", &got, err)
	}
}

func TestNewProtoMessageWithoutCorrelationID(t *testing.T) {
	m, err := NewProtoMessage(context.Background(), "s", wrapperspb.String("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Headers[HeaderCorrelationID]; ok {
		t.Errorf("unexpected correlation header: %v", m.Headers)
	}
}
