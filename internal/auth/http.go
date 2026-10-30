package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/imaging"
	"github.com/google/uuid"
)

const (
	accessCookie  = "rc_at"
	refreshCookie = "rc_rt"
	// The refresh cookie is only sent to auth endpoints, never to the rest of the API.
	refreshCookiePath = "/api/v1/auth"
)

type Handler struct {
	svc          *Service
	secureCookie bool
}

// NewHandler builds the HTTP layer. secureCookie should be false only in dev,
// where the API is served over plain http on localhost.
func NewHandler(svc *Service, secureCookie bool) *Handler {
	return &Handler{svc: svc, secureCookie: secureCookie}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/register", httpx.Handle(h.register))
	mux.Handle("POST /api/v1/auth/login", httpx.Handle(h.login))
	mux.Handle("POST /api/v1/auth/refresh", httpx.Handle(h.refresh))
	mux.Handle("POST /api/v1/auth/logout", httpx.Handle(h.logout))
	mux.Handle("GET /api/v1/users/@me", h.RequireAuth(httpx.Handle(h.me)))
	mux.Handle("PATCH /api/v1/users/@me", h.RequireAuth(httpx.Handle(h.updateMe)))
	mux.Handle("DELETE /api/v1/users/@me", h.RequireAuth(httpx.Handle(h.deleteMe)))
	mux.Handle("PUT /api/v1/users/@me/avatar", h.RequireAuth(httpx.Handle(h.setAvatar)))
	mux.Handle("DELETE /api/v1/users/@me/avatar", h.RequireAuth(httpx.Handle(h.deleteAvatar)))
	mux.Handle("POST /api/v1/gateway/ticket", h.RequireAuth(httpx.Handle(h.ticket)))
}

// maxDevice matches the sessions.device CHECK (characters, not bytes).
const maxDevice = 128

// clientInfo describes the device from the User-Agent. Headers are raw bytes:
// invalid UTF-8 or NUL would make Postgres reject the session row, so they
// are dropped, and the length is cut by characters, never mid-character.
func clientInfo(r *http.Request) ClientInfo {
	ua := strings.ReplaceAll(strings.ToValidUTF8(r.UserAgent(), ""), "\x00", "")
	if runes := []rune(ua); len(runes) > maxDevice {
		ua = string(runes[:maxDevice])
	}
	return ClientInfo{IP: httpx.ClientIP(r), Device: ua}
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	var in RegisterInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	t, err := h.svc.Register(r.Context(), in, clientInfo(r))
	if err != nil {
		return err
	}
	h.setCookies(w, t)
	httpx.WriteJSON(w, http.StatusCreated, t.User)
	return nil
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var in LoginInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	t, err := h.svc.Login(r.Context(), in, clientInfo(r))
	if err != nil {
		return err
	}
	h.setCookies(w, t)
	httpx.WriteJSON(w, http.StatusOK, t.User)
	return nil
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		h.clearCookies(w)
		return ErrSessionExpired
	}
	t, err := h.svc.Refresh(r.Context(), c.Value)
	if err != nil {
		if err == ErrSessionExpired { //nolint:errorlint // sentinel returned as-is
			h.clearCookies(w)
		}
		return err
	}
	h.setCookies(w, t)
	httpx.WriteJSON(w, http.StatusOK, t.User)
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(refreshCookie); err == nil {
		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			return err
		}
	}
	h.clearCookies(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	u, err := h.svc.Me(r.Context(), PrincipalFrom(r.Context()))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) error {
	var in UpdateMeInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	u, err := h.svc.UpdateMe(r.Context(), PrincipalFrom(r.Context()), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (h *Handler) deleteMe(w http.ResponseWriter, r *http.Request) error {
	var in DeleteMeInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	if err := h.svc.DeleteMe(r.Context(), PrincipalFrom(r.Context()), in); err != nil {
		return err
	}
	h.clearCookies(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) setAvatar(w http.ResponseWriter, r *http.Request) error {
	upload, err := httpx.ReadBody(w, r, imaging.MaxUploadBytes)
	if err != nil {
		return err
	}
	u, err := h.svc.SetAvatar(r.Context(), PrincipalFrom(r.Context()), upload)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (h *Handler) deleteAvatar(w http.ResponseWriter, r *http.Request) error {
	u, err := h.svc.DeleteAvatar(r.Context(), PrincipalFrom(r.Context()))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (h *Handler) ticket(w http.ResponseWriter, r *http.Request) error {
	t, err := h.svc.IssueTicket(r.Context(), PrincipalFrom(r.Context()))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": t, "expires_in": int(ticketTTL / time.Second)})
	return nil
}

// Secure is off only in dev (plain http on localhost); see NewHandler.
func (h *Handler) setCookies(w http.ResponseWriter, t Tokens) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configurable, HttpOnly and SameSite are set
		Name: accessCookie, Value: t.Access, Path: "/", MaxAge: int(accessTTL / time.Second),
		HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configurable, HttpOnly and SameSite are set
		Name: refreshCookie, Value: t.Refresh, Path: refreshCookiePath, MaxAge: int(refreshTTL / time.Second),
		HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearCookies(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{{accessCookie, "/"}, {refreshCookie, refreshCookiePath}} {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configurable, HttpOnly and SameSite are set
			Name: c.name, Value: "", Path: c.path, MaxAge: -1,
			HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteLaxMode,
		})
	}
}

type principalKey struct{}

// RequireAuth rejects requests without a valid access cookie and puts the
// Principal into the request context.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(accessCookie)
		if err != nil {
			httpx.WriteError(w, r, httpx.ErrUnauthorized)
			return
		}
		p, err := h.svc.Authenticate(c.Value)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// UserID is the caller's id on a request that passed RequireAuth.
func UserID(r *http.Request) uuid.UUID { return PrincipalFrom(r.Context()).UserID }

// PrincipalFrom returns the caller set by RequireAuth.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}
