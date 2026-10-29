package social

import (
	"context"
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
		"GET /api/v1/users/@me/relationships":           h.listRelationships,
		"POST /api/v1/users/@me/relationships":          h.requestByUsername,
		"PUT /api/v1/users/@me/relationships/{user}":    h.putRelationship,
		"DELETE /api/v1/users/@me/relationships/{user}": h.deleteRelationship,

		"GET /api/v1/users/@me/channels":                      h.listDMs,
		"POST /api/v1/users/@me/channels":                     h.createDM,
		"PATCH /api/v1/users/@me/channels/{channel}":          h.renameGroup,
		"DELETE /api/v1/users/@me/channels/{channel}":         h.leaveGroup,
		"PUT /api/v1/channels/{channel}/recipients/{user}":    h.addRecipient,
		"DELETE /api/v1/channels/{channel}/recipients/{user}": h.removeRecipient,
	}
	for pattern, fn := range routes {
		mux.Handle(pattern, h.requireAuth(httpx.Handle(fn)))
	}
}

func (h *Handler) listRelationships(w http.ResponseWriter, r *http.Request) error {
	rels, err := h.svc.List(r.Context(), auth.UserID(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, rels)
	return nil
}

func (h *Handler) requestByUsername(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.RequestByUsername(r.Context(), auth.UserID(r), in.Username))
}

func (h *Handler) putRelationship(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "user")
	if err != nil {
		return err
	}
	var in struct {
		Type RelType `json:"type"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	switch in.Type {
	case 0, RelFriend:
		return httpx.NoContent(w, h.svc.Request(r.Context(), auth.UserID(r), id))
	case RelBlocked:
		return httpx.NoContent(w, h.svc.Block(r.Context(), auth.UserID(r), id))
	default:
		return httpx.Validation(map[string]string{"type": "1 (в друзья) или 2 (заблокировать)"})
	}
}

func (h *Handler) deleteRelationship(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "user")
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.Remove(r.Context(), auth.UserID(r), id))
}

func (h *Handler) listDMs(w http.ResponseWriter, r *http.Request) error {
	ds, err := h.svc.ListDMs(r.Context(), auth.UserID(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ds)
	return nil
}

// createDM opens a 1:1 DM with {recipient_id} or creates a group with {recipients, name?}.
func (h *Handler) createDM(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		RecipientID *uuid.UUID `json:"recipient_id"`
		GroupInput
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	var (
		d   DM
		err error
	)
	switch {
	case in.RecipientID != nil && in.Recipients == nil && in.Name == nil:
		d, err = h.svc.OpenDM(r.Context(), auth.UserID(r), *in.RecipientID)
	case in.RecipientID == nil:
		d, err = h.svc.CreateGroup(r.Context(), auth.UserID(r), in.GroupInput)
	default:
		err = httpx.Validation(map[string]string{"recipient_id": "Либо recipient_id, либо recipients"})
	}
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, d)
	return nil
}

func (h *Handler) renameGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	var in struct {
		Name *string `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	d, err := h.svc.RenameGroup(r.Context(), auth.UserID(r), id, in.Name)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, d)
	return nil
}

func (h *Handler) leaveGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	return httpx.NoContent(w, h.svc.RemoveRecipient(r.Context(), auth.UserID(r), id, auth.UserID(r)))
}

func (h *Handler) addRecipient(w http.ResponseWriter, r *http.Request) error {
	return h.recipient(w, r, h.svc.AddRecipient)
}

func (h *Handler) removeRecipient(w http.ResponseWriter, r *http.Request) error {
	return h.recipient(w, r, h.svc.RemoveRecipient)
}

func (h *Handler) recipient(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, me, id, user uuid.UUID) error) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	user, err := httpx.PathUUID(r, "user")
	if err != nil {
		return err
	}
	return httpx.NoContent(w, fn(r.Context(), auth.UserID(r), ch, user))
}
