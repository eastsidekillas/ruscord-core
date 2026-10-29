package httpx

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
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

// ClientIP returns the peer address. Proxy headers are not trusted yet:
// behind a reverse proxy this must be extended with a trusted-proxy list.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
