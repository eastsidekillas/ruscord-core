package message

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

func reactions(ch, msg string) string { return msgs(ch) + "/" + msg + "/reactions" }

func react(ch, msg, emoji string) string {
	return reactions(ch, msg) + "/" + url.PathEscape(emoji) + "/@me"
}

// shown is what a reader sees under a message: "emoji:count:me" each.
func shown(m map[string]any) []string {
	out := []string{}
	for _, r := range list(m["reactions"]) {
		r := obj(r)
		me := "-"
		if r["me"] == true {
			me = "me"
		}
		n, _ := r["count"].(float64)
		out = append(out, str(r["emoji"])+":"+strconv.Itoa(int(n))+":"+me)
	}
	return out
}

func TestEmojiList(t *testing.T) {
	for _, ok := range []string{"👍", "👍🏽", "❤️", "🏳️‍🌈", "🇷🇺", "🧑‍🧑‍🧒"} {
		if !emojiSet[ok] {
			t.Errorf("%q must be a reaction", ok)
		}
	}
	for _, bad := range []string{"", "x", "👍👍", "🏽", "🇷", "❤", " 👍", "<:custom:1>"} {
		if emojiSet[bad] {
			t.Errorf("%q must not be a reaction", bad)
		}
	}
	if len(emojiSet) < 3900 {
		t.Fatalf("only %d emoji", len(emojiSet))
	}
}

func TestReactions(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, text, _ := e.server(owner, bob)
	m := str(send(owner, text, "голосуем")["id"])
	history := func(u *apitest.User) map[string]any {
		return obj(list(u.Expect(http.StatusOK, "GET", msgs(text)+"?limit=1", nil))[0])
	}

	bob.Expect(http.StatusUnprocessableEntity, "PUT", react(text, m, "x"), nil)
	bob.Expect(http.StatusUnprocessableEntity, "PUT", react(text, m, "👍👍"), nil)
	eve.Expect(http.StatusNotFound, "PUT", react(text, m, "👍"), nil)
	bob.Expect(http.StatusNoContent, "PUT", react(text, m, "👍"), nil)
	bob.Expect(http.StatusNoContent, "PUT", react(text, m, "👍"), nil) // again: nothing changes
	if n := len(e.Outbox("MESSAGE_REACTION_ADD")); n != 1 {
		t.Fatalf("%d MESSAGE_REACTION_ADD, want 1", n)
	}
	if ev := e.Outbox("MESSAGE_REACTION_ADD")[0]; ev.Data["emoji"] != "👍" || ev.Data["user_id"] != bob.ID.String() || ev.Data["message_id"] != m {
		t.Fatalf("event = %v", ev.Data)
	}
	if got := shown(history(bob)); strings.Join(got, " ") != "👍:1:me" {
		t.Fatalf("bob sees %v", got)
	}
	if got := shown(history(owner)); strings.Join(got, " ") != "👍:1:-" {
		t.Fatalf("owner sees %v", got)
	}

	// Without ADD_REACTIONS a member may only join what is there.
	ow := "/api/v1/channels/" + text + "/permissions/" + gid
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.AddReactions)})
	bob.Expect(http.StatusForbidden, "PUT", react(text, m, "🔥"), nil)
	owner.Expect(http.StatusNoContent, "PUT", react(text, m, "🔥"), nil)
	bob.Expect(http.StatusNoContent, "PUT", react(text, m, "🔥"), nil)
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": "0"})
	if got := shown(history(bob)); strings.Join(got, " ") != "👍:1:me 🔥:2:me" {
		t.Fatalf("after 🔥: %v", got)
	}
	people := list(bob.Expect(http.StatusOK, "GET", reactions(text, m)+"/"+url.PathEscape("🔥"), nil))
	if len(people) != 2 || obj(people[0])["id"] != owner.ID.String() || obj(people[1])["id"] != bob.ID.String() {
		t.Fatalf("who put 🔥: %v", people)
	}

	// At most 20 different emoji under one message.
	if _, err := e.PG.Exec(t.Context(),
		`INSERT INTO message_reactions (message_id, emoji, user_id)
		 SELECT $1, e, $2 FROM unnest($3::text[]) AS e`, m, owner.ID,
		[]string{"😀", "😃", "😄", "😁", "😆", "😅", "🤣", "😂", "🙂", "🙃", "🫠", "😉", "😊", "😇", "🥰", "😍", "🤩", "😘"}); err != nil {
		t.Fatal(err)
	}
	bob.Expect(http.StatusUnprocessableEntity, "PUT", react(text, m, "🎉"), nil)
	bob.Expect(http.StatusNoContent, "PUT", react(text, m, "😀"), nil) // joining one of the 20 is fine

	// Taking one's own away.
	bob.Expect(http.StatusNoContent, "DELETE", react(text, m, "👍"), nil)
	bob.Expect(http.StatusNoContent, "DELETE", react(text, m, "👍"), nil)
	if n := len(e.Outbox("MESSAGE_REACTION_REMOVE")); n != 1 {
		t.Fatalf("%d MESSAGE_REACTION_REMOVE, want 1", n)
	}

	// Moderation: one emoji or all of them, MANAGE_MESSAGES only.
	bob.Expect(http.StatusForbidden, "DELETE", reactions(text, m)+"/"+url.PathEscape("🔥"), nil)
	bob.Expect(http.StatusForbidden, "DELETE", reactions(text, m), nil)
	owner.Expect(http.StatusNoContent, "DELETE", reactions(text, m)+"/"+url.PathEscape("🔥"), nil)
	if ev := e.Outbox("MESSAGE_REACTION_REMOVE_EMOJI"); len(ev) != 1 || ev[0].Data["emoji"] != "🔥" {
		t.Fatalf("remove emoji events: %v", ev)
	}
	owner.Expect(http.StatusNoContent, "DELETE", reactions(text, m), nil)
	if len(e.Outbox("MESSAGE_REACTION_REMOVE_ALL")) != 1 || len(shown(history(bob))) != 0 {
		t.Fatalf("after clearing: %v", shown(history(bob)))
	}
	// Deleting the message takes its reactions.
	bob.Expect(http.StatusNoContent, "PUT", react(text, m, "👍"), nil)
	owner.Expect(http.StatusNoContent, "DELETE", msgs(text)+"/"+m, nil)
	var left int
	_ = e.PG.QueryRow(t.Context(), `SELECT count(*) FROM message_reactions`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d reactions left", left)
	}
}

func TestReactionsAndPinsInDM(t *testing.T) {
	e := newEnv(t)
	ann, bob := e.User("ann"), e.User("bob")
	dm := e.dm(ann, bob)
	m := str(send(ann, dm, "привет")["id"])
	bob.Expect(http.StatusNoContent, "PUT", react(dm, m, "❤️"), nil)
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/channels/"+dm+"/pins/"+m, nil)
	bob.Expect(http.StatusForbidden, "DELETE", reactions(dm, m), nil) // no moderation in DMs
	// A block takes reactions and pins away, even joining one that is there.
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{"type": 2})
	ann.Expect(http.StatusForbidden, "PUT", react(dm, m, "❤️"), nil)
	ann.Expect(http.StatusForbidden, "DELETE", "/api/v1/channels/"+dm+"/pins/"+m, nil)
	if pins := list(ann.Expect(http.StatusOK, "GET", "/api/v1/channels/"+dm+"/pins", nil)); len(pins) != 1 {
		t.Fatalf("pins after a block: %v", pins)
	}
}

func TestPins(t *testing.T) {
	e := newEnv(t)
	owner, bob, mod := e.User("owner"), e.User("bob"), e.User("mod")
	gid, text, _ := e.server(owner, bob, mod)
	g := "/api/v1/guilds/" + gid
	role := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "pins", "permissions": permStr(perm.PinMessages)}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+mod.ID.String()+"/roles/"+role, nil)
	pins := "/api/v1/channels/" + text + "/pins"
	first, second := str(send(bob, text, "первое")["id"]), str(send(bob, text, "второе")["id"])

	bob.Expect(http.StatusForbidden, "PUT", pins+"/"+first, nil) // PIN_MESSAGES is not in @everyone
	mod.Expect(http.StatusNoContent, "PUT", pins+"/"+first, nil)
	mod.Expect(http.StatusNoContent, "PUT", pins+"/"+first, nil) // already pinned: nothing changes
	mod.Expect(http.StatusNoContent, "PUT", pins+"/"+second, nil)
	if n := len(e.Outbox("MESSAGE_UPDATE")); n != 2 {
		t.Fatalf("%d MESSAGE_UPDATE, want 2", n)
	}
	got := list(bob.Expect(http.StatusOK, "GET", pins, nil))
	if len(got) != 2 || obj(got[0])["id"] != second || obj(got[0])["pinned_at"] == nil {
		t.Fatalf("pins = %v", got)
	}
	if h := obj(list(bob.Expect(http.StatusOK, "GET", msgs(text)+"?limit=1", nil))[0]); h["pinned_at"] == nil || h["edited_at"] != nil {
		t.Fatalf("a pinned message in the history: %v", h)
	}
	mod.Expect(http.StatusNoContent, "DELETE", pins+"/"+second, nil)
	if got := list(bob.Expect(http.StatusOK, "GET", pins, nil)); len(got) != 1 || obj(got[0])["id"] != first {
		t.Fatalf("after unpinning: %v", got)
	}
	mod.Expect(http.StatusNotFound, "PUT", pins+"/00000000-0000-7000-8000-000000000000", nil)

	// At most 250 pinned messages in a channel.
	if _, err := e.PG.Exec(t.Context(),
		`INSERT INTO messages (id, channel_id, guild_id, author_id, content, pinned_at)
		 SELECT gen_random_uuid(), $1, $2, $3, 'x', now() FROM generate_series(1, 249)`, text, gid, bob.ID); err != nil {
		t.Fatal(err)
	}
	mod.Expect(http.StatusUnprocessableEntity, "PUT", pins+"/"+second, nil)
}
