package guild

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

type Handler struct {
	svc         *Service
	requireAuth func(http.Handler) http.Handler
}

// NewHandler builds the HTTP layer; requireAuth puts the auth.Principal into
// the request context.
func NewHandler(svc *Service, requireAuth func(http.Handler) http.Handler) *Handler {
	return &Handler{svc: svc, requireAuth: requireAuth}
}

func (h *Handler) Register(mux *http.ServeMux) {
	routes := map[string]func(http.ResponseWriter, *http.Request) error{
		"GET /api/v1/users/@me/guilds":  h.listMine,
		"POST /api/v1/guilds":           h.create,
		"GET /api/v1/guilds/{guild}":    h.get,
		"PATCH /api/v1/guilds/{guild}":  h.update,
		"DELETE /api/v1/guilds/{guild}": h.delete,

		"GET /api/v1/guilds/{guild}/channels":                    h.listChannels,
		"POST /api/v1/guilds/{guild}/channels":                   h.createChannel,
		"PATCH /api/v1/channels/{channel}":                       h.updateChannel,
		"DELETE /api/v1/channels/{channel}":                      h.deleteChannel,
		"PUT /api/v1/channels/{channel}/permissions/{target}":    h.putOverwrite,
		"DELETE /api/v1/channels/{channel}/permissions/{target}": h.deleteOverwrite,

		"GET /api/v1/guilds/{guild}/roles":           h.listRoles,
		"POST /api/v1/guilds/{guild}/roles":          h.createRole,
		"PATCH /api/v1/guilds/{guild}/roles/{role}":  h.updateRole,
		"DELETE /api/v1/guilds/{guild}/roles/{role}": h.deleteRole,

		"GET /api/v1/guilds/{guild}/members":                        h.listMembers,
		"PATCH /api/v1/guilds/{guild}/members/@me":                  h.updateSelf,
		"DELETE /api/v1/guilds/{guild}/members/{user}":              h.removeMember,
		"PUT /api/v1/guilds/{guild}/members/{user}/roles/{role}":    h.addRole,
		"DELETE /api/v1/guilds/{guild}/members/{user}/roles/{role}": h.removeRole,
	}
	routes["POST /api/v1/channels/{channel}/invites"] = h.createInvite
	routes["GET /api/v1/guilds/{guild}/invites"] = h.listInvites
	routes["GET /api/v1/invites/{code}"] = h.previewInvite
	routes["POST /api/v1/invites/{code}"] = h.acceptInvite
	routes["DELETE /api/v1/invites/{code}"] = h.deleteInvite
	for pattern, fn := range routes {
		mux.Handle(pattern, h.requireAuth(httpx.Handle(fn)))
	}
}

func me(r *http.Request) uuid.UUID { return auth.PrincipalFrom(r.Context()).UserID }

// pathID parses a path id. A malformed id is just another missing object.
func pathID(r *http.Request, name string) (uuid.UUID, error) {
	v := r.PathValue(name)
	if v == "@me" && name == "user" {
		return me(r), nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func pathIDs(r *http.Request, names ...string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(names))
	for i, n := range names {
		id, err := pathID(r, n)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) error {
	gs, err := h.svc.ListMine(r.Context(), me(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, gs)
	return nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	var in CreateGuildInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	f, err := h.svc.Create(r.Context(), me(r), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, f)
	return nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	f, err := h.svc.Get(r.Context(), me(r), id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, f)
	return nil
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	var in UpdateGuildInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	g, err := h.svc.Update(r.Context(), me(r), id, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, g)
	return nil
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context(), me(r), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) listChannels(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	chs, err := h.svc.ListChannels(r.Context(), me(r), id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, chs)
	return nil
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	var in CreateChannelInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	ch, err := h.svc.CreateChannel(r.Context(), me(r), id, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, ch)
	return nil
}

func (h *Handler) updateChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "channel")
	if err != nil {
		return err
	}
	var in UpdateChannelInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	ch, err := h.svc.UpdateChannel(r.Context(), me(r), id, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ch)
	return nil
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "channel")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteChannel(r.Context(), me(r), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) putOverwrite(w http.ResponseWriter, r *http.Request) error {
	ids, err := pathIDs(r, "channel", "target")
	if err != nil {
		return err
	}
	var in OverwriteInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	ch, err := h.svc.PutOverwrite(r.Context(), me(r), ids[0], ids[1], in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ch)
	return nil
}

func (h *Handler) deleteOverwrite(w http.ResponseWriter, r *http.Request) error {
	ids, err := pathIDs(r, "channel", "target")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteOverwrite(r.Context(), me(r), ids[0], ids[1]); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	roles, err := h.svc.ListRoles(r.Context(), me(r), id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, roles)
	return nil
}

func (h *Handler) createRole(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	var in RoleInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	role, err := h.svc.CreateRole(r.Context(), me(r), id, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, role)
	return nil
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) error {
	ids, err := pathIDs(r, "guild", "role")
	if err != nil {
		return err
	}
	var in RoleInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	role, err := h.svc.UpdateRole(r.Context(), me(r), ids[0], ids[1], in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, role)
	return nil
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) error {
	ids, err := pathIDs(r, "guild", "role")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteRole(r.Context(), me(r), ids[0], ids[1]); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	var after uuid.UUID
	if v := q.Get("after"); v != "" {
		if after, err = uuid.Parse(v); err != nil {
			return httpx.NewError(http.StatusBadRequest, "invalid_request", "after должен быть id пользователя")
		}
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return httpx.NewError(http.StatusBadRequest, "invalid_request", "limit от 1 до 1000")
		}
	}
	ms, err := h.svc.ListMembers(r.Context(), me(r), id, after, limit)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ms)
	return nil
}

func (h *Handler) updateSelf(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	var in UpdateSelfInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	if err := h.svc.UpdateSelf(r.Context(), me(r), id, in); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) error {
	ids, err := pathIDs(r, "guild", "user")
	if err != nil {
		return err
	}
	if err := h.svc.RemoveMember(r.Context(), me(r), ids[0], ids[1]); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) addRole(w http.ResponseWriter, r *http.Request) error { return h.setRole(w, r, true) }
func (h *Handler) removeRole(w http.ResponseWriter, r *http.Request) error {
	return h.setRole(w, r, false)
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request, add bool) error {
	ids, err := pathIDs(r, "guild", "user", "role")
	if err != nil {
		return err
	}
	if err := h.svc.SetMemberRole(r.Context(), me(r), ids[0], ids[1], ids[2], add); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "channel")
	if err != nil {
		return err
	}
	var in CreateInviteInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	inv, err := h.svc.CreateInvite(r.Context(), me(r), id, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, inv)
	return nil
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "guild")
	if err != nil {
		return err
	}
	invs, err := h.svc.ListInvites(r.Context(), me(r), id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, invs)
	return nil
}

func (h *Handler) previewInvite(w http.ResponseWriter, r *http.Request) error {
	p, err := h.svc.PreviewInvite(r.Context(), me(r), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) error {
	f, err := h.svc.AcceptInvite(r.Context(), me(r), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, f)
	return nil
}

func (h *Handler) deleteInvite(w http.ResponseWriter, r *http.Request) error {
	if err := h.svc.DeleteInvite(r.Context(), me(r), r.PathValue("code")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
