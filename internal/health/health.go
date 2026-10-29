// Package health serves liveness (/healthz) and readiness (/readyz) probes.
// Liveness only says the process is up; readiness checks every dependency.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check returns nil when the dependency is usable.
type Check func(ctx context.Context) error

type Handler struct {
	checks  map[string]Check
	timeout time.Duration
	verbose bool
}

// New creates a probe handler. With verbose=false failing checks report only
// "fail", so error text (hosts, ports, usernames) never leaves the process.
func New(timeout time.Duration, verbose bool) *Handler {
	return &Handler{checks: map[string]Check{}, timeout: timeout, verbose: verbose}
}

func (h *Handler) Add(name string, c Check) { h.checks[name] = c }

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", h.ready)
}

type result struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	names := make([]string, 0, len(h.checks))
	for n := range h.checks {
		names = append(names, n)
	}
	sort.Strings(names)

	res := result{Status: "ok", Checks: make(map[string]string, len(names))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string, c Check) {
			defer wg.Done()
			status := "ok"
			if err := c(ctx); err != nil {
				status = "fail"
				if h.verbose {
					status = err.Error()
				}
			}
			mu.Lock()
			res.Checks[name] = status
			if status != "ok" {
				res.Status = "unavailable"
			}
			mu.Unlock()
		}(n, h.checks[n])
	}
	wg.Wait()

	code := http.StatusOK
	if res.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, res)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
