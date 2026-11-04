package message

import (
	"net/http"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// states reads the user's read states by channel id.
func states(u *apitest.User) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, s := range list(u.Expect(http.StatusOK, "GET", "/api/v1/users/@me/read-states", nil)) {
		out[str(obj(s)["channel_id"])] = obj(s)
	}
	return out
}

func ackPath(ch, msg string) string { return msgs(ch) + "/" + msg + "/ack" }

func TestReadStates(t *testing.T) {
	e := newEnv(t)
	owner, bob, ann, eve := e.User("owner"), e.User("bob"), e.User("ann"), e.User("eve")
	gid, text, voice := e.server(owner, bob, ann)
	g := "/api/v1/guilds/" + gid
	dm := e.dm(bob, ann)

	// Nothing written yet: known channels, nothing read, nothing to read.
	st := states(bob)
	if len(st) != 2 || st[text] == nil || st[dm] == nil || st[voice] != nil {
		t.Fatalf("read states = %v", st)
	}
	if st[text]["last_read_id"] != nil || st[text]["last_message_id"] != nil || st[text]["mention_count"] != float64(0) {
		t.Fatalf("empty channel = %v", st[text])
	}

	// Mentions: by name, by a role bob has, @everyone; plain text is not one.
	role := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "игроки", "mentionable": true}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+bob.ID.String()+"/roles/"+role, nil)
	first := str(send(owner, text, "привет "+mention(bob))["id"])
	send(owner, text, "<@&"+role+"> играем?")
	third := str(send(owner, text, "@everyone сбор")["id"])
	last := str(send(owner, text, "просто текст")["id"])
	st = states(bob)
	if st[text]["last_message_id"] != last || st[text]["last_read_id"] != nil || st[text]["mention_count"] != float64(3) {
		t.Fatalf("bob after four messages = %v", st[text])
	}
	// The author has read their own messages.
	if o := states(owner)[text]; o["last_read_id"] != last || o["mention_count"] != float64(0) {
		t.Fatalf("author = %v", o)
	}

	// A mark moves forward only, and tells bob's other sessions when it moves.
	bob.Expect(http.StatusNoContent, "POST", ackPath(text, third), nil)
	if st := states(bob)[text]; st["last_read_id"] != third || st["mention_count"] != float64(0) {
		t.Fatalf("after reading the third = %v", st)
	}
	bob.Expect(http.StatusNoContent, "POST", ackPath(text, first), nil)
	if st := states(bob)[text]; st["last_read_id"] != third {
		t.Fatalf("a mark went back: %v", st)
	}
	acks := e.Outbox("MESSAGE_ACK")
	if len(acks) != 1 || acks[0].Topic != "user."+bob.ID.String() || acks[0].Data["message_id"] != third || acks[0].Data["channel_id"] != text {
		t.Fatalf("MESSAGE_ACK = %v", acks)
	}
	bob.Expect(http.StatusNotFound, "POST", ackPath(text, "00000000-0000-7000-8000-000000000000"), nil)
	bob.Expect(http.StatusNotFound, "POST", ackPath(dm, first), nil) // a message of another channel
	eve.Expect(http.StatusNotFound, "POST", ackPath(text, last), nil)

	// The whole guild as read.
	send(owner, text, mention(bob)+" ещё")
	eve.Expect(http.StatusNotFound, "POST", g+"/ack", nil)
	bob.Expect(http.StatusNoContent, "POST", g+"/ack", nil)
	if st := states(bob)[text]; st["last_read_id"] != st["last_message_id"] || st["mention_count"] != float64(0) {
		t.Fatalf("after reading the guild = %v", st)
	}
	if n := len(e.Outbox("MESSAGE_ACK")); n != 2 {
		t.Fatalf("%d MESSAGE_ACK after reading the guild", n)
	}

	// A channel bob may not read is not his to know about.
	ow := "/api/v1/channels/" + text + "/permissions/" + gid
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.ReadMessageHistory)})
	if _, ok := states(bob)[text]; ok {
		t.Fatal("a channel without READ_MESSAGE_HISTORY is in the read states")
	}
	bob.Expect(http.StatusForbidden, "POST", ackPath(text, last), nil)

	// DMs count too; a DM has no roles or @everyone.
	send(ann, dm, "лично "+mention(bob))
	if st := states(bob)[dm]; st["mention_count"] != float64(1) || st["last_message_id"] == nil {
		t.Fatalf("DM = %v", st)
	}

	// Deleting a channel deletes the marks in it.
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+text, nil)
	var n int
	_ = e.PG.QueryRow(t.Context(), `SELECT count(*) FROM read_states WHERE channel_id = $1`, text).Scan(&n)
	if n != 0 {
		t.Fatalf("%d marks of a deleted channel", n)
	}
}
