// Package health provides liveness and readiness HTTP endpoints.
//
// /healthz reports that the process is up. /readyz runs the registered checks
// (database, broker, ...) and reports 503 until all of them pass, so
// orchestrators only route traffic to instances that can serve it.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check reports whether a dependency is usable. A nil error means healthy.
type Check func(ctx context.Context) error

// Handler serves health endpoints.
type Handler struct {
	timeout time.Duration

	mu     sync.RWMutex
	checks map[string]Check
}

// New returns a Handler. Each readiness check is given timeout to complete.
func New(timeout time.Duration) *Handler {
	return &Handler{timeout: timeout, checks: make(map[string]Check)}
}

// AddReadiness registers a named readiness check.
func (h *Handler) AddReadiness(name string, c Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = c
}

// Routes returns a mux exposing /healthz and /readyz.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", h.ready)
	return mux
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	names := make([]string, 0, len(h.checks))
	for n := range h.checks {
		names = append(names, n)
	}
	h.mu.RUnlock()
	sort.Strings(names)

	status := http.StatusOK
	results := make(map[string]string, len(names))
	for _, n := range names {
		h.mu.RLock()
		check := h.checks[n]
		h.mu.RUnlock()

		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		err := check(ctx)
		cancel()

		if err != nil {
			status = http.StatusServiceUnavailable
			results[n] = err.Error()
			continue
		}
		results[n] = "ok"
	}

	body := map[string]any{"checks": results}
	if status == http.StatusOK {
		body["status"] = "ready"
	} else {
		body["status"] = "unavailable"
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
