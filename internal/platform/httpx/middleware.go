package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
)

// Recover turns a panic in a handler into a 500 instead of a dropped connection.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				slog.ErrorContext(r.Context(), "panic", "value", v, "stack", string(debug.Stack()))
				WriteError(w, r, NewError(http.StatusInternalServerError, "internal", "Внутренняя ошибка"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CheckOrigin rejects state-changing requests whose Origin header names a
// site outside allowed. Together with SameSite=Lax cookies this is the CSRF
// defence. Requests without Origin (non-browser clients) pass.
func CheckOrigin(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if o := r.Header.Get("Origin"); o != "" && !set[o] {
				WriteError(w, r, NewError(http.StatusForbidden, "bad_origin", "Запрос с чужого сайта отклонён"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type clientIPKey struct{}

// TrustProxies makes ClientIP see through the reverse proxies in trusted.
// A request from a trusted peer is attributed to the rightmost address in
// X-Forwarded-For that is not itself trusted: each proxy appends the address
// it got the request from, and anything left of the first untrusted hop may
// have been sent by the client. Requests from other peers keep their peer
// address, so a client cannot claim an address by sending the header.
func TrustProxies(trusted []netip.Prefix, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddr(peerHost(r))
		if err != nil || !isTrusted(peer.Unmap()) {
			next.ServeHTTP(w, r)
			return
		}
		client := peer
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break // a malformed hop: trust nothing further left
			}
			client = a.Unmap()
			if !isTrusted(client) {
				break
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, client.String())))
	})
}

// ClientIP returns the address of the client: the peer, or what
// TrustProxies found behind trusted proxies.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return peerHost(r)
}

func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
