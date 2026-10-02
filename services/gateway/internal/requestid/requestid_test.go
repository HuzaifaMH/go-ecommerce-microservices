package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestContextRoundTrip(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Fatalf("From(empty) = %q", got)
	}
	if got := From(With(context.Background(), "req-1")); got != "req-1" {
		t.Fatalf("From = %q", got)
	}
}

func TestNewIsUniqueAndValid(t *testing.T) {
	a, b := New(), New()
	if a == b || !Valid(a) {
		t.Fatalf("New() = %q, %q", a, b)
	}
}

func TestValid(t *testing.T) {
	tests := map[string]bool{
		"abc-123_X.y":                          true,
		"550e8400-e29b-41d4-a716-446655440000": true,
		strings.Repeat("a", MaxLength):         true,
		"":                                     false,
		strings.Repeat("a", MaxLength+1):       false,
		"has space":                            false,
		"line\nbreak":                          false,
		"carriage\rreturn":                     false,
		"semi;colon":                           false,
		"quote\"":                              false,
		"unicode-é":                            false,
		"<script>":                             false,
	}
	for id, want := range tests {
		if got := Valid(id); got != want {
			t.Errorf("Valid(%q) = %v, want %v", id, got, want)
		}
	}
}
