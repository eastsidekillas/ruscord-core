package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// event is one message of the EVENTS stream.
type event struct {
	Subject string
	Type    string
	Data    json.RawMessage
	ID      int64 // outbox id (the stream message id); 0 when unknown
}

// dispatch is a DISPATCH for the client before it gets a sequence number.
type dispatch struct {
	T string
	D json.RawMessage
}

// routed is what one stream event means for one session.
type routed struct {
	out []dispatch
	// resubscribe: the set of topics changed (joined or left a guild or a
	// DM), so the consumer must restart right after this event.
	resubscribe bool
	// revoked: the sign-in session behind the connection was revoked.
	revoked bool
}

// GuildLoader loads one guild as userID sees it; guild.Service implements it.
type GuildLoader interface {
	Snapshot(ctx context.Context, userID, guildID uuid.UUID) (guild.Snapshot, error)
}

// router decides which events a session receives. It keeps a snapshot of
// every guild of the user and updates it from the events themselves, so
// filtering a message is a map lookup and a permission change turns into
// CHANNEL_CREATE or CHANNEL_DELETE for exactly the channels it reveals or
// hides. Hidden channels never leave the router.
type router struct {
	user        uuid.UUID
	authSession uuid.UUID
	guilds      map[uuid.UUID]*guildState
	dms         map[uuid.UUID]bool
	loader      GuildLoader
}

func newRouter(user, authSession uuid.UUID, loader GuildLoader, snaps []guild.Snapshot, dmIDs []uuid.UUID) *router {
	r := &router{user: user, authSession: authSession, loader: loader,
		guilds: make(map[uuid.UUID]*guildState, len(snaps)), dms: make(map[uuid.UUID]bool, len(dmIDs))}
	for _, s := range snaps {
		r.guilds[s.Guild.ID] = newGuildState(s)
	}
	for _, id := range dmIDs {
		r.dms[id] = true
	}
	return r
}

// subjects are the stream subjects the session consumes, in a stable order.
func (r *router) subjects() []string {
	out := make([]string, 0, 1+len(r.guilds)+len(r.dms))
	out = append(out, events.UserTopic(r.user))
	for id := range r.guilds {
		out = append(out, events.GuildTopic(id))
	}
	for id := range r.dms {
		out = append(out, events.ChannelTopic(id))
	}
	slices.Sort(out[1:])
	return out
}

// route applies ev to the session state. An error means the state could not
// be kept consistent (the database failed while loading a guild); the
// session must end, because skipping the event would leave the client
// without that guild.
func (r *router) route(ctx context.Context, ev event) (routed, error) {
	kind, id, ok := strings.Cut(ev.Subject, ".")
	if !ok {
		return routed{}, nil
	}
	topicID, err := uuid.Parse(id)
	if err != nil {
		return routed{}, nil
	}
	switch kind {
	case "user":
		if topicID != r.user {
			return routed{}, nil
		}
		return r.userEvent(ctx, ev)
	case "guild":
		g := r.guilds[topicID]
		if g == nil {
			return routed{}, nil
		}
		return r.guildEvent(topicID, g, ev)
	case "channel":
		if r.dms[topicID] && isMessage(ev.Type) {
			return forward(ev), nil
		}
	}
	return routed{}, nil
}

func forward(ev event) routed { return routed{out: []dispatch{{T: ev.Type, D: ev.Data}}} }

func isMessage(t string) bool {
	return t == events.MessageCreate || t == events.MessageUpdate || t == events.MessageDelete
}

type idPayload struct {
	ID uuid.UUID `json:"id"`
}

func (r *router) userEvent(ctx context.Context, ev event) (routed, error) {
	switch ev.Type {
	case events.RelationshipAdd, events.RelationshipRemove,
		events.RecipientAdd, events.RecipientRemove, events.ChannelUpdate:
		return forward(ev), nil

	case events.GuildCreate:
		var p idPayload
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		// The payload was rendered when the event was written; the router
		// needs every channel, so it loads the guild as it is now and
		// renders GUILD_CREATE from that. Guild events the fresh snapshot
		// already contains are skipped by its cursor (see guildEvent).
		snap, err := r.loader.Snapshot(ctx, r.user, p.ID)
		if errors.Is(err, httpx.ErrNotFound) {
			return routed{}, nil // already left; GUILD_DELETE follows
		}
		if err != nil {
			return routed{}, fmt.Errorf("load guild %s: %w", p.ID, err)
		}
		_, known := r.guilds[p.ID]
		g := newGuildState(snap)
		r.guilds[p.ID] = g
		return routed{out: []dispatch{{T: events.GuildCreate, D: mustJSON(g.full())}}, resubscribe: !known}, nil

	case events.GuildDelete:
		var p idPayload
		if json.Unmarshal(ev.Data, &p) != nil || r.guilds[p.ID] == nil {
			return routed{}, nil
		}
		delete(r.guilds, p.ID)
		return routed{out: []dispatch{{T: ev.Type, D: ev.Data}}, resubscribe: true}, nil

	case events.ChannelCreate, events.ChannelDelete: // direct messages
		var p idPayload
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		create := ev.Type == events.ChannelCreate
		changed := r.dms[p.ID] != create
		if create {
			r.dms[p.ID] = true
		} else {
			delete(r.dms, p.ID)
		}
		return routed{out: []dispatch{{T: ev.Type, D: ev.Data}}, resubscribe: changed}, nil

	case events.SessionRevoke:
		var p struct {
			SessionID uuid.UUID `json:"session_id"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.SessionID == r.authSession {
			return routed{revoked: true}, nil
		}
	}
	return routed{}, nil
}

// guildEvent applies an event of a guild's topic. Whenever it changes what
// the user may do in the guild, GUILD_PERMISSIONS_UPDATE follows the event's
// own dispatches with the new values, so clients never compute permissions
// (docs/adr/0017-client-permissions.md).
func (r *router) guildEvent(guildID uuid.UUID, g *guildState, ev event) (routed, error) {
	if ev.ID != 0 && ev.ID <= g.snap.Cursor && !isMessage(ev.Type) {
		return routed{}, nil // the snapshot already has this change
	}
	before := g.view
	res, err := r.applyGuildEvent(guildID, g, ev)
	if err == nil && r.guilds[guildID] == g && !g.view.Equal(before) {
		res.out = append(res.out, g.permissionsUpdate())
	}
	return res, err
}

func (r *router) applyGuildEvent(guildID uuid.UUID, g *guildState, ev event) (routed, error) {
	switch ev.Type {
	case events.GuildUpdate:
		var p guild.Guild
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		g.snap.Guild = p // the owner may have changed
		return g.withDiff(forward(ev)), nil

	case events.GuildDelete:
		delete(r.guilds, guildID)
		return routed{out: []dispatch{{T: ev.Type, D: ev.Data}}, resubscribe: true}, nil

	case events.ChannelCreate, events.ChannelUpdate:
		var ch guild.Channel
		if json.Unmarshal(ev.Data, &ch) != nil {
			return routed{}, nil
		}
		_, before := g.view.Channels[ch.ID]
		g.upsert(ch)
		_, after := g.view.Channels[ch.ID]
		switch {
		case before && after:
			return routed{out: []dispatch{{T: events.ChannelUpdate, D: ev.Data}}}, nil
		case after:
			return routed{out: []dispatch{{T: events.ChannelCreate, D: ev.Data}}}, nil
		case before:
			return routed{out: []dispatch{channelGone(guildID, ch.ID)}}, nil
		}
		return routed{}, nil

	case events.ChannelDelete:
		var p idPayload
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		_, visible := g.view.Channels[p.ID]
		g.remove(p.ID)
		if visible {
			return forward(ev), nil
		}
		return routed{}, nil

	case events.RoleCreate, events.RoleUpdate, events.RoleDelete:
		var p struct {
			Roles []guild.Role `json:"roles"`
		}
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		g.snap.Roles = p.Roles
		return g.withDiff(forward(ev)), nil

	case events.MemberUpdate:
		var m guild.Member
		if json.Unmarshal(ev.Data, &m) != nil {
			return routed{}, nil
		}
		if m.UserID != r.user {
			return forward(ev), nil
		}
		g.snap.Member = m
		return g.withDiff(forward(ev)), nil

	case events.MemberAdd, events.MemberRemove:
		return forward(ev), nil

	case events.InviteCreate, events.InviteDelete:
		if g.view.Base.Has(perm.ManageGuild) {
			return forward(ev), nil
		}

	case events.MessageCreate, events.MessageUpdate, events.MessageDelete:
		var p struct {
			ChannelID uuid.UUID `json:"channel_id"`
		}
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		if _, ok := g.view.Channels[p.ChannelID]; ok {
			return forward(ev), nil
		}
	}
	return routed{}, nil
}

// channelGone tells the client a channel left its view; it has the same
// shape as a real CHANNEL_DELETE, so the client handles both the same way.
func channelGone(guildID, channelID uuid.UUID) dispatch {
	return dispatch{T: events.ChannelDelete, D: mustJSON(map[string]uuid.UUID{"id": channelID, "guild_id": guildID})}
}

// permissionsData is the payload of GUILD_PERMISSIONS_UPDATE: the guild-wide
// permissions and those in every visible channel, replacing earlier values.
type permissionsData struct {
	GuildID            uuid.UUID                      `json:"guild_id"`
	Permissions        perm.Permissions               `json:"permissions"`
	ChannelPermissions map[uuid.UUID]perm.Permissions `json:"channel_permissions"`
}

func (g *guildState) permissionsUpdate() dispatch {
	return dispatch{T: typePermissionsUpdate, D: mustJSON(permissionsData{
		GuildID: g.snap.Guild.ID, Permissions: g.view.Base, ChannelPermissions: g.view.Channels,
	})}
}

// guildState is one guild of a session: its snapshot and the member's view.
type guildState struct {
	snap guild.Snapshot
	view guild.View
}

func newGuildState(s guild.Snapshot) *guildState {
	return &guildState{snap: s, view: s.View()}
}

func (g *guildState) full() guild.Full { return g.snap.Full(g.view) }

func (g *guildState) upsert(ch guild.Channel) {
	i := slices.IndexFunc(g.snap.Channels, func(c guild.Channel) bool { return c.ID == ch.ID })
	if i >= 0 {
		g.snap.Channels[i] = ch
	} else {
		g.snap.Channels = append(g.snap.Channels, ch)
	}
	g.view = g.snap.View()
}

func (g *guildState) remove(id uuid.UUID) {
	g.snap.Channels = slices.DeleteFunc(g.snap.Channels, func(c guild.Channel) bool { return c.ID == id })
	g.view = g.snap.View()
}

// withDiff recomputes the view after a guild-wide change (roles, own
// member, owner) and appends a CHANNEL_CREATE for every channel that became
// visible and a CHANNEL_DELETE for every channel that became hidden.
func (g *guildState) withDiff(r routed) routed {
	old := g.view
	g.view = g.snap.View()
	for _, ch := range g.snap.Channels {
		_, was := old.Channels[ch.ID]
		_, is := g.view.Channels[ch.ID]
		switch {
		case is && !was:
			r.out = append(r.out, dispatch{T: events.ChannelCreate, D: mustJSON(ch)})
		case was && !is:
			r.out = append(r.out, channelGone(g.snap.Guild.ID, ch.ID))
		}
	}
	return r
}

// mustJSON marshals values built by this package; they always marshal.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("gateway: marshal %T: %v", v, err))
	}
	return b
}
