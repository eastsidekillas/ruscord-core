// Package metrics exposes process metrics in the Prometheus text format
// (docs/adr/0022-metrics-backups.md). It is deliberately small: counters and
// histograms by a few labels, and gauges read at scrape time. No dependency:
// the format is a few lines of text.
package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds every metric of the process.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
	names   map[string]bool
}

type metric interface {
	write(w io.Writer)
}

func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

func (r *Registry) add(name string, m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic("metrics: duplicate " + name)
	}
	r.names[name] = true
	r.metrics = append(r.metrics, m)
}

// Write writes every metric in the text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// Handler serves the registry at GET /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// labels is a fixed list of label names; values come in the same order.
type labels []string

func (l labels) format(values []string, extra ...string) string {
	if len(l) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(l)+len(extra)/2)
	for i, name := range l {
		parts = append(parts, name+`="`+escape(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func header(w io.Writer, name, help, typ string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func number(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Counter counts events by label values.
type Counter struct {
	name, help string
	labels     labels
	mu         sync.Mutex
	values     map[string]float64
	keys       map[string][]string
}

func (r *Registry) Counter(name, help string, labelNames ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labelNames, values: map[string]float64{}, keys: map[string][]string{}}
	r.add(name, c)
	return c
}

// Add adds v (≥ 0) for the label values, in the order of the label names.
func (c *Counter) Add(v float64, values ...string) {
	k := strings.Join(values, "\xff")
	c.mu.Lock()
	if _, ok := c.keys[k]; !ok {
		c.keys[k] = slices.Clone(values)
	}
	c.values[k] += v
	c.mu.Unlock()
}

func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

func (c *Counter) write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range sortedKeys(c.values) {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", c.name, c.labels.format(c.keys[k]), number(c.values[k]))
	}
}

// Histogram counts observations into buckets by label values.
type Histogram struct {
	name, help string
	labels     labels
	bounds     []float64
	mu         sync.Mutex
	series     map[string]*hseries
}

type hseries struct {
	values []string
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// DurationBuckets suit request latencies, in seconds.
var DurationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func (r *Registry) Histogram(name, help string, bounds []float64, labelNames ...string) *Histogram {
	h := &Histogram{name: name, help: help, labels: labelNames, bounds: bounds, series: map[string]*hseries{}}
	r.add(name, h)
	return h
}

func (h *Histogram) Observe(v float64, values ...string) {
	k := strings.Join(values, "\xff")
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		s = &hseries{values: slices.Clone(values), counts: make([]uint64, len(h.bounds))}
		h.series[k] = s
	}
	if i, _ := slices.BinarySearch(h.bounds, v); i < len(h.bounds) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
}

func (h *Histogram) write(w io.Writer) {
	header(w, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		var cum uint64
		for i, b := range h.bounds {
			cum += s.counts[i]
			_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labels.format(s.values, "le", number(b)), cum)
		}
		_, _ = fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labels.format(s.values, "le", "+Inf"), s.count)
		_, _ = fmt.Fprintf(w, "%s_sum%s %s\n", h.name, h.labels.format(s.values), number(s.sum))
		_, _ = fmt.Fprintf(w, "%s_count%s %d\n", h.name, h.labels.format(s.values), s.count)
	}
}

// GaugeFunc is read when scraped. A read that fails is logged and skipped,
// so one broken source does not hide the others.
type GaugeFunc struct {
	name, help string
	read       func(ctx context.Context) (float64, error)
	log        *slog.Logger
}

func (r *Registry) GaugeFunc(name, help string, log *slog.Logger, read func(ctx context.Context) (float64, error)) {
	r.add(name, &GaugeFunc{name: name, help: help, read: read, log: log})
}

func (g *GaugeFunc) write(w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := g.read(ctx)
	if err != nil {
		g.log.Warn("metrics: gauge read", "name", g.name, "err", err)
		return
	}
	header(w, g.name, g.help, "gauge")
	_, _ = fmt.Fprintf(w, "%s %s\n", g.name, number(v))
}

// Runtime registers the Go runtime gauges and the build version.
func (r *Registry) Runtime(version string, log *slog.Logger) {
	start := float64(time.Now().Unix())
	r.GaugeFunc("ruscord_start_time_seconds", "Unix time the process started.", log,
		func(context.Context) (float64, error) { return start, nil })
	r.GaugeFunc("go_goroutines", "Goroutines that currently exist.", log,
		func(context.Context) (float64, error) { return float64(runtime.NumGoroutine()), nil })
	r.GaugeFunc("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", log,
		func(context.Context) (float64, error) {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return float64(m.HeapAlloc), nil
		})
	info := r.Counter("ruscord_build_info", "Always 1; the version label names the build.", "version")
	info.Add(1, version)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// HTTP counts requests and their latency by route pattern and status class.
// The route is the ServeMux pattern ("GET /api/v1/guilds/{guild}"), never the
// raw path: ids in paths would make a series per object.
func HTTP(r *Registry, next http.Handler) http.Handler {
	reqs := r.Counter("ruscord_http_requests_total", "HTTP requests by route and status class.", "route", "code")
	dur := r.Histogram("ruscord_http_request_duration_seconds", "HTTP request latency by route.", DurationBuckets, "route")
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, req)
		route := req.Pattern
		if route == "" {
			route = "unmatched"
		}
		reqs.Inc(route, strconv.Itoa(sw.code/100)+"xx")
		dur.Observe(time.Since(start).Seconds(), route)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.code, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (the gateway
// hijacks it for the WebSocket).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
