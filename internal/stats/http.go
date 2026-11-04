package stats

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

type Handler struct {
	svc         *Service
	requireAuth func(http.Handler) http.Handler
	userID      func(*http.Request) uuid.UUID
}

// NewHandler serves reports to signed-in admins; userID reads the user of
// a request that passed requireAuth (auth.UserID).
func NewHandler(svc *Service, requireAuth func(http.Handler) http.Handler, userID func(*http.Request) uuid.UUID) *Handler {
	return &Handler{svc: svc, requireAuth: requireAuth, userID: userID}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/admin/stats", h.requireAuth(httpx.Handle(h.report)))
	mux.Handle("POST /api/v1/visits", httpx.Handle(h.visit))
}

func (h *Handler) report(w http.ResponseWriter, r *http.Request) error {
	days := 0
	if s := r.URL.Query().Get("days"); s != "" {
		var err error
		if days, err = strconv.Atoi(s); err != nil {
			return errDays
		}
	}
	rep, err := h.svc.Report(r.Context(), h.userID(r), days)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, rep)
	return nil
}

type visitInput struct {
	Ref string `json:"ref"`
}

func (h *Handler) visit(w http.ResponseWriter, r *http.Request) error {
	var in visitInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	if err := h.svc.Visit(r.Context(), httpx.ClientIP(r), in.Ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
