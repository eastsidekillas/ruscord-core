package metrics

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
)

func scrape(r *Registry) string {
	var b bytes.Buffer
	r.Write(&b)
	return b.String()
}

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Things.", "kind")
	c.Inc("a")
	c.Add(2, `quo"te`)
	h := r.Histogram("lat_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "r")
	h.Observe(0.5, "r")
	h.Observe(5, "r")
	h.Observe(1, "r") // a bound belongs to its bucket (le)
	r.GaugeFunc("ok_gauge", "Fine.", testdb.Logger(), func(context.Context) (float64, error) { return 7, nil })
	r.GaugeFunc("broken_gauge", "Fails.", testdb.Logger(), func(context.Context) (float64, error) { return 0, errors.New("down") })

	got := scrape(r)
	for _, want := range []string{
		"# TYPE x_total counter\n",
		`x_total{kind="a"} 1` + "\n",
		`x_total{kind="quo\"te"} 2` + "\n",
		"# TYPE lat_seconds histogram\n",
		`lat_seconds_bucket{route="r",le="0.1"} 1` + "\n",
		`lat_seconds_bucket{route="r",le="1"} 3` + "\n",
		`lat_seconds_bucket{route="r",le="+Inf"} 4` + "\n",
		`lat_seconds_sum{route="r"} 6.55` + "\n",
		`lat_seconds_count{route="r"} 4` + "\n",
		"ok_gauge 7\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "broken_gauge") {
		t.Error("a failing gauge must be skipped, not reported as 0")
	}
	defer func() {
		if recover() == nil {
			t.Error("a duplicate name must panic")
		}
	}()
	r.Counter("x_total", "again")
}

// Routes are ServeMux patterns, never raw paths with ids.
func TestHTTPMiddleware(t *testing.T) {
	r := NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux.HandleFunc("POST /ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	h := HTTP(r, mux)
	for _, req := range []*http.Request{
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/things/1", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/things/2", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ok", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nowhere", nil),
	} {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	got := scrape(r)
	for _, want := range []string{
		`ruscord_http_requests_total{route="GET /things/{id}",code="4xx"} 2`,
		`ruscord_http_requests_total{route="POST /ok",code="2xx"} 1`,
		`ruscord_http_requests_total{route="unmatched",code="4xx"} 1`,
		`ruscord_http_request_duration_seconds_count{route="GET /things/{id}"} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "/things/1") {
		t.Error("a raw path leaked into the labels")
	}
}
