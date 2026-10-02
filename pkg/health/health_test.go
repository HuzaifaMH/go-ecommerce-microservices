package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestLivenessAlwaysOK(t *testing.T) {
	h := New(time.Second)
	h.AddReadiness("db", func(context.Context) error { return errors.New("down") })

	if rec := get(t, h.Routes(), "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}

func TestReadiness(t *testing.T) {
	tests := []struct {
		name   string
		checks map[string]Check
		want   int
	}{
		{"no checks", nil, http.StatusOK},
		{"all healthy", map[string]Check{"db": func(context.Context) error { return nil }}, http.StatusOK},
		{"one failing", map[string]Check{
			"db":   func(context.Context) error { return nil },
			"nats": func(context.Context) error { return errors.New("connection refused") },
		}, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := New(time.Second)
			for n, c := range tc.checks {
				h.AddReadiness(n, c)
			}
			if rec := get(t, h.Routes(), "/readyz"); rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestReadinessCheckTimesOut(t *testing.T) {
	h := New(20 * time.Millisecond)
	h.AddReadiness("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	if rec := get(t, h.Routes(), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503", rec.Code)
	}
}
