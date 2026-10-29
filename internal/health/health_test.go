package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serve(t *testing.T, h *Handler, path string) (*httptest.ResponseRecorder, result) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	var res result
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := New(time.Second, true)
	h.Add("db", func(context.Context) error { return errors.New("down") })
	rec, _ := serve(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestReadyzAllOK(t *testing.T) {
	h := New(time.Second, true)
	h.Add("db", func(context.Context) error { return nil })
	h.Add("cache", func(context.Context) error { return nil })
	rec, res := serve(t, h, "/readyz")
	if rec.Code != http.StatusOK || res.Status != "ok" {
		t.Fatalf("code = %d status = %q, want 200 ok", rec.Code, res.Status)
	}
}

func TestReadyzReportsFailingDependency(t *testing.T) {
	h := New(time.Second, true)
	h.Add("db", func(context.Context) error { return nil })
	h.Add("nats", func(context.Context) error { return errors.New("not connected") })
	rec, res := serve(t, h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if res.Checks["nats"] != "not connected" || res.Checks["db"] != "ok" {
		t.Fatalf("checks = %v", res.Checks)
	}
}

func TestReadyzRespectsTimeout(t *testing.T) {
	h := New(20*time.Millisecond, true)
	h.Add("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	start := time.Now()
	rec, _ := serve(t, h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if time.Since(start) > time.Second {
		t.Fatal("readiness check ignored the timeout")
	}
}

func TestReadyzHidesErrorsWhenNotVerbose(t *testing.T) {
	h := New(time.Second, false)
	h.Add("postgres", func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") })
	_, res := serve(t, h, "/readyz")
	if res.Checks["postgres"] != "fail" {
		t.Fatalf("error text leaked: %q", res.Checks["postgres"])
	}
}
