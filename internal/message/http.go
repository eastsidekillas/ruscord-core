package message

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

type Handler struct {
	svc         *Service
	requireAuth func(http.Handler) http.Handler
}

func NewHandler(svc *Service, requireAuth func(http.Handler) http.Handler) *Handler {
	return &Handler{svc: svc, requireAuth: requireAuth}
}

func (h *Handler) Register(mux *http.ServeMux) {
	routes := map[string]func(http.ResponseWriter, *http.Request) error{
		"POST /api/v1/channels/{channel}/messages":             h.send,
		"GET /api/v1/channels/{channel}/messages":              h.history,
		"PATCH /api/v1/channels/{channel}/messages/{message}":  h.edit,
		"DELETE /api/v1/channels/{channel}/messages/{message}": h.delete,
	}
	for pattern, fn := range routes {
		mux.Handle(pattern, h.requireAuth(httpx.Handle(fn)))
	}
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	var in SendInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	m, created, err := h.svc.Send(r.Context(), auth.UserID(r), ch, in)
	if err != nil {
		return err
	}
	status := http.StatusOK // a retried nonce
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, m)
	return nil
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	var pg Page
	if pg.Before, err = httpx.QueryUUID(q, "before", "id сообщения"); err != nil {
		return err
	}
	if pg.After, err = httpx.QueryUUID(q, "after", "id сообщения"); err != nil {
		return err
	}
	if pg.Before != nil && pg.After != nil {
		return httpx.NewError(http.StatusBadRequest, "invalid_request", "Укажите before или after, не оба")
	}
	if pg.Limit, err = httpx.QueryLimit(q, maxLimit); err != nil {
		return err
	}
	ms, err := h.svc.History(r.Context(), auth.UserID(r), ch, pg)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ms)
	return nil
}

func (h *Handler) ids(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "message")
	return ch, id, err
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	var in struct {
		Content string `json:"content"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	m, err := h.svc.Edit(r.Context(), auth.UserID(r), ch, id, in.Content)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, m)
	return nil
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Delete(r.Context(), auth.UserID(r), ch, id))
}
