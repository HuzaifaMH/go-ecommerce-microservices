package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestNewJSONIncludesServiceAndRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, Config{Service: "order", Level: "warn", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("hidden")
	log.Warn("shown", "order_id", "o-1")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not a single JSON record: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "shown" || rec["service"] != "order" || rec["order_id"] != "o-1" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestRecordsInATraceCarryItsIDs(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, Config{Service: "order", Level: "info", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	log.InfoContext(ctx, "inside a trace")
	log.Info("no context, so no trace")
	log.With("order_id", "o-1").InfoContext(ctx, "child logger keeps the behaviour")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %s", len(lines), buf.String())
	}
	var in, out, child map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &in)
	_ = json.Unmarshal([]byte(lines[1]), &out)
	_ = json.Unmarshal([]byte(lines[2]), &child)

	if in["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || in["span_id"] != "00f067aa0ba902b7" {
		t.Errorf("a record logged inside a trace must carry its IDs: %v", in)
	}
	if _, ok := out["trace_id"]; ok {
		t.Errorf("a record without a trace context must not get IDs: %v", out)
	}
	if child["trace_id"] != in["trace_id"] || child["order_id"] != "o-1" || child["service"] != "order" {
		t.Errorf("With() must keep adding trace IDs: %v", child)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []Config{
		{Level: "loud", Format: "json"},
		{Level: "info", Format: "xml"},
	}
	for _, c := range tests {
		if _, err := New(&bytes.Buffer{}, c); err == nil {
			t.Errorf("New(%+v) expected error", c)
		}
	}
}
