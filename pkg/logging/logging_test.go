package logging

import (
	"bytes"
	"encoding/json"
	"testing"
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
