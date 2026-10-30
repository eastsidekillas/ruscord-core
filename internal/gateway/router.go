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
	"github.com/eastsidekillas/ruscord-core/internal/presence"
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
	// ephChanged: the ephemeral subjects changed (a friend added or removed;
	// resubscribe implies it too).
	ephChanged bool
	// choiceChanged: the user chose another status; presence must be
	// recomputed.
	choiceChanged bool
	// newFriends: people who became friends; the client needs their current
	// status, since PRESENCE_UPDATE only reports changes.
	newFriends []uuid.UUID
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

	// friends get the user's presence and send theirs (eph.presence.<id>).
	friends map[uuid.UUID]bool
	// choice is the status the user chose (USER_UPDATE changes it).
	choice presence.Status
	// seen is the last status delivered per user. One change reaches the
	// session once per shared guild and once more if a friend; only the
	// first copy goes out.
	seen map[uuid.UUID]presence.Status
}

func newRouter(user, authSession uuid.UUID, loader GuildLoader, snaps []guild.Snapshot, dmIDs []uuid.UUID) *router {
	r := &router{user: user, authSession: authSession, loader: loader,
		guilds: make(map[uuid.UUID]*guildState, len(snaps)), dms: make(map[uuid.UUID]bool, len(dmIDs)),
		friends: map[uuid.UUID]bool{}, choice: presence.Online, seen: map[uuid.UUID]presence.Status{}}
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

// ephSubjects are the ephemeral subjects the session listens to.
func (r *router) ephSubjects() []string {
	out := make([]string, 0, len(r.guilds)+len(r.dms)+len(r.friends))
	for id := range r.guilds {
		out = append(out, events.EphGuildTopic(id))
	}
	for id := range r.dms {
		out = append(out, events.EphChannelTopic(id))
	}
	for id := range r.friends {
		out = append(out, events.EphPresenceTopic(id))
	}
	slices.Sort(out)
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
	case events.RecipientAdd, events.RecipientRemove, events.ChannelUpdate:
		return forward(ev), nil

	case events.RelationshipAdd, events.RelationshipRemove:
		res := forward(ev)
		var p struct {
			ID   uuid.UUID `json:"id"`
			Type int       `json:"type"`
		}
		if json.Unmarshal(ev.Data, &p) != nil || p.Type != relFriend {
			return res, nil
		}
		was := r.friends[p.ID]
		if ev.Type == events.RelationshipAdd {
			r.friends[p.ID] = true
			if !was {
				res.newFriends = []uuid.UUID{p.ID}
			}
		} else {
			delete(r.friends, p.ID)
			delete(r.seen, p.ID) // a stale status must not suppress the next one
		}
		res.ephChanged = was != r.friends[p.ID]
		return res, nil

	case events.UserUpdate:
		var p struct {
			Status presence.Status `json:"status"`
		}
		if json.Unmarshal(ev.Data, &p) != nil {
			return routed{}, nil
		}
		res := forward(ev)
		if presence.ValidChoice(p.Status) && p.Status != r.choice {
			r.choice = p.Status
			res.choiceChanged = true
		}
		return res, nil

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
		r.forgetSeen()
		return routed{out: []dispatch{{T: events.GuildCreate, D: mustJSON(g.full())}}, resubscribe: !known}, nil

	case events.GuildDelete:
		var p idPayload
		if json.Unmarshal(ev.Data, &p) != nil || r.guilds[p.ID] == nil {
			return routed{}, nil
		}
		delete(r.guilds, p.ID)
		r.forgetSeen()
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
		r.forgetSeen()
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

// relFriend is social.RelFriend as it travels in RELATIONSHIP_* payloads.
const relFriend = 1

// routeEphemeral filters an ephemeral event (typing, presence). It reads
// the session state and only changes the record of delivered statuses.
func (r *router) routeEphemeral(ev event) routed {
	rest, ok := strings.CutPrefix(ev.Subject, "eph.")
	if !ok {
		return routed{}
	}
	kind, id, ok := strings.Cut(rest, ".")
	if !ok {
		return routed{}
	}
	topicID, err := uuid.Parse(id)
	if err != nil {
		return routed{}
	}
	switch ev.Type {
	case events.TypingStart:
		var p struct {
			ChannelID uuid.UUID `json:"channel_id"`
			UserID    uuid.UUID `json:"user_id"`
		}
		if json.Unmarshal(ev.Data, &p) != nil || p.UserID == r.user {
			return routed{}
		}
		switch kind {
		case "guild":
			g := r.guilds[topicID]
			if g == nil {
				return routed{}
			}
			if _, visible := g.view.Channels[p.ChannelID]; !visible {
				return routed{} // typing in a hidden channel reveals it exists
			}
		case "channel":
			if !r.dms[topicID] || p.ChannelID != topicID {
				return routed{}
			}
		default:
			return routed{}
		}
		return forward(ev)

	case events.PresenceUpdate:
		var p presence.Update
		if json.Unmarshal(ev.Data, &p) != nil || p.UserID == r.user {
			return routed{}
		}
		switch kind {
		case "guild":
			if r.guilds[topicID] == nil {
				return routed{}
			}
		case "presence":
			if !r.friends[topicID] || p.UserID != topicID {
				return routed{}
			}
		default:
			return routed{}
		}
		return r.presence(p)
	}
	return routed{}
}

// forgetSeen drops the record of delivered statuses when the set of guilds
// changes: a status missed while not sharing a guild would otherwise
// suppress the next real one. A repeated status is harmless; a suppressed
// one leaves a person shown in the wrong state.
func (r *router) forgetSeen() { clear(r.seen) }

// presence delivers a status unless this one was the last delivered.
func (r *router) presence(p presence.Update) routed {
	if r.seen[p.UserID] == p.Status {
		return routed{}
	}
	r.seen[p.UserID] = p.Status
	return routed{out: []dispatch{{T: events.PresenceUpdate, D: mustJSON(p)}}}
}
