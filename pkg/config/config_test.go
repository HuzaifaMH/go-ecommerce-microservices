package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoaderValues(t *testing.T) {
	l := FromMap(map[string]string{
		"NAME":    "orders",
		"PORT":    "8080",
		"DEBUG":   "true",
		"TIMEOUT": "1500ms",
	})

	if got := l.String("NAME", "x"); got != "orders" {
		t.Errorf("String = %q", got)
	}
	if got := l.String("MISSING", "fallback"); got != "fallback" {
		t.Errorf("String default = %q", got)
	}
	if got := l.Int("PORT", 1); got != 8080 {
		t.Errorf("Int = %d", got)
	}
	if got := l.Bool("DEBUG", false); !got {
		t.Errorf("Bool = %v", got)
	}
	if got := l.Duration("TIMEOUT", time.Second); got != 1500*time.Millisecond {
		t.Errorf("Duration = %v", got)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoaderAccumulatesAllErrors(t *testing.T) {
	l := FromMap(map[string]string{
		"PORT":    "abc",
		"DEBUG":   "maybe",
		"TIMEOUT": "soon",
	})
	l.RequiredString("DATABASE_URL")
	l.Int("PORT", 1)
	l.Bool("DEBUG", false)
	l.Duration("TIMEOUT", time.Second)

	err := l.Err()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL is required", "PORT", "DEBUG", "TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRequiredStringPresent(t *testing.T) {
	l := FromMap(map[string]string{"DATABASE_URL": "postgres://x"})
	if got := l.RequiredString("DATABASE_URL"); got != "postgres://x" {
		t.Errorf("got %q", got)
	}
	if err := l.Err(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
