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
		"POST /api/v1/channels/{channel}/typing":               h.typing,
		"POST /api/v1/channels/{channel}/attachments":          h.upload,

		"PUT /api/v1/channels/{channel}/messages/{message}/reactions/{emoji}/@me":    h.react,
		"DELETE /api/v1/channels/{channel}/messages/{message}/reactions/{emoji}/@me": h.unreact,
		"GET /api/v1/channels/{channel}/messages/{message}/reactions/{emoji}":        h.reactors,
		"DELETE /api/v1/channels/{channel}/messages/{message}/reactions/{emoji}":     h.clearEmoji,
		"DELETE /api/v1/channels/{channel}/messages/{message}/reactions":             h.clearAll,
		"GET /api/v1/channels/{channel}/pins":                                        h.pins,
		"PUT /api/v1/channels/{channel}/pins/{message}":                              h.pin,
		"DELETE /api/v1/channels/{channel}/pins/{message}":                           h.unpin,

		"GET /api/v1/users/@me/read-states":                      h.readStates,
		"POST /api/v1/channels/{channel}/messages/{message}/ack": h.ack,
		"POST /api/v1/guilds/{guild}/ack":                        h.ackGuild,
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

func (h *Handler) readStates(w http.ResponseWriter, r *http.Request) error {
	states, err := h.svc.ReadStates(r.Context(), auth.UserID(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, states)
	return nil
}

func (h *Handler) ack(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Ack(r.Context(), auth.UserID(r), ch, id))
}

func (h *Handler) ackGuild(w http.ResponseWriter, r *http.Request) error {
	g, err := httpx.PathUUID(r, "guild")
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.AckGuild(r.Context(), auth.UserID(r), g))
}

func (h *Handler) react(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.React(r.Context(), auth.UserID(r), ch, id, r.PathValue("emoji")))
}

func (h *Handler) unreact(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Unreact(r.Context(), auth.UserID(r), ch, id, r.PathValue("emoji")))
}

func (h *Handler) clearEmoji(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	if r.PathValue("emoji") == "" {
		return httpx.ErrNotFound
	}
	return httpx.NoContent(w, h.svc.ClearReactions(r.Context(), auth.UserID(r), ch, id, r.PathValue("emoji")))
}

func (h *Handler) clearAll(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.ClearReactions(r.Context(), auth.UserID(r), ch, id, ""))
}

func (h *Handler) reactors(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	limit, err := httpx.QueryLimit(r.URL.Query(), maxReactors)
	if err != nil {
		return err
	}
	users, err := h.svc.Reactors(r.Context(), auth.UserID(r), ch, id, r.PathValue("emoji"), limit)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, users)
	return nil
}

func (h *Handler) pins(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	ms, err := h.svc.Pins(r.Context(), auth.UserID(r), ch)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ms)
	return nil
}

func (h *Handler) pin(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Pin(r.Context(), auth.UserID(r), ch, id, true))
}

func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) error {
	ch, id, err := h.ids(r)
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Pin(r.Context(), auth.UserID(r), ch, id, false))
}

// upload takes the file as the request body, its name in ?filename= and
// its size in Content-Length (ADR-0036).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	body := http.MaxBytesReader(w, r.Body, MaxAttachmentBytes)
	a, err := h.svc.Upload(r.Context(), auth.UserID(r), ch, r.URL.Query().Get("filename"), body, r.ContentLength)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
	return nil
}

func (h *Handler) typing(w http.ResponseWriter, r *http.Request) error {
	channelID, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Typing(r.Context(), auth.UserID(r), channelID))
}
