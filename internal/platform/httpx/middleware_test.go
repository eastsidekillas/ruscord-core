package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestTrustProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	cases := []struct {
		name, peer string
		xff        []string
		want       string
	}{
		{"direct client, no header", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"untrusted peer cannot claim an address", "203.0.113.7:5000", []string{"1.2.3.4"}, "203.0.113.7"},
		{"behind one trusted proxy", "10.0.0.2:80", []string{"198.51.100.9"}, "198.51.100.9"},
		{"client-sent value left of the real hop is ignored", "10.0.0.2:80", []string{"1.2.3.4, 198.51.100.9"}, "198.51.100.9"},
		{"chain of trusted proxies", "10.0.0.2:80", []string{"198.51.100.9, 10.0.0.5"}, "198.51.100.9"},
		{"header split over lines", "10.0.0.2:80", []string{"1.2.3.4", "198.51.100.9"}, "198.51.100.9"},
		{"trusted proxy without header", "10.0.0.2:80", nil, "10.0.0.2"},
		{"only trusted hops: the leftmost", "10.0.0.2:80", []string{"10.0.0.9"}, "10.0.0.9"},
		{"malformed hop stops the walk", "10.0.0.2:80", []string{"198.51.100.9, junk"}, "10.0.0.2"},
		{"IPv6 loopback proxy", "[::1]:80", []string{"2001:db8::1"}, "2001:db8::1"},
		{"IPv4-mapped IPv6 peer", "[::ffff:10.0.0.2]:80", []string{"198.51.100.9"}, "198.51.100.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := TrustProxies(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIP(r) }))
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = c.peer
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != c.want {
				t.Fatalf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
	// Without trusted proxies the header is never read.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:80"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	var got string
	TrustProxies(nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIP(r) })).
		ServeHTTP(httptest.NewRecorder(), r)
	if got != "10.0.0.2" {
		t.Fatalf("no trusted proxies: %q", got)
	}
}

func TestRecover(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	var body Error
	_ = json.NewDecoder(w.Body).Decode(&body)
	if w.Code != http.StatusInternalServerError || body.Code != "internal" || strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("panic answered %d %+v", w.Code, body)
	}
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler must propagate, got %v", v)
		}
	}()
	Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
}
