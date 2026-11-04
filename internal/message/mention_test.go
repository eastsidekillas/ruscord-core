package message

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

func TestParseMentions(t *testing.T) {
	u1, u2 := uuid.New(), uuid.New()
	r1 := uuid.New()
	user := func(id uuid.UUID) string { return "<@" + id.String() + ">" }
	role := func(id uuid.UUID) string { return "<@&" + id.String() + ">" }
	many := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(user(uuid.New()))
		}
		return b.String()
	}
	cases := []struct {
		name     string
		text     string
		everyone bool
		users    []uuid.UUID
		roles    []uuid.UUID
		invalid  bool
	}{
		{name: "nothing", text: "привет"},
		{name: "everyone", text: "@everyone смотрите", everyone: true},
		{name: "here", text: "эй @here", everyone: true},
		{name: "everyone as part of a word", text: "@everyoneX"},
		{name: "users in order, once", text: user(u2) + " и " + user(u1) + user(u2), users: []uuid.UUID{u2, u1}},
		{name: "role", text: "внимание " + role(r1), roles: []uuid.UUID{r1}},
		{name: "role is not a user", text: role(r1)},
		{name: "upper case id is text", text: "<@" + strings.ToUpper(u1.String()) + ">"},
		{name: "broken id", text: "<@1234>"},
		{name: "inline code", text: "`" + user(u1) + " @everyone`"},
		{name: "double backticks", text: "``a ` " + user(u1) + "``"},
		{name: "code block", text: "```\n" + user(u1) + "\n@here\n```", users: nil},
		{name: "after code", text: "`код` " + user(u1), users: []uuid.UUID{u1}},
		{name: "unclosed backtick", text: "` " + user(u1), users: []uuid.UUID{u1}},
		{name: "fifty", text: many(maxMentions), users: nil},
		{name: "too many", text: many(maxMentions) + role(r1), invalid: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parseMentions(c.text)
			if c.invalid {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.everyone != c.everyone {
				t.Fatalf("everyone = %v", p.everyone)
			}
			if c.name == "fifty" {
				if len(p.users) != maxMentions {
					t.Fatalf("users = %d", len(p.users))
				}
				return
			}
			if !slices.Equal(p.users, c.users) && (len(p.users) != 0 || len(c.users) != 0) {
				t.Fatalf("users = %v, want %v", p.users, c.users)
			}
			if !slices.Equal(p.roles, c.roles) && (len(p.roles) != 0 || len(c.roles) != 0) {
				t.Fatalf("roles = %v, want %v", p.roles, c.roles)
			}
		})
	}
}

// ids reads a JSON array of ids.
func ids(v any) []string {
	out := []string{}
	for _, x := range list(v) {
		out = append(out, str(x))
	}
	return out
}

func mention(u *apitest.User) string { return "<@" + u.ID.String() + ">" }

func TestMentions(t *testing.T) {
	e := newEnv(t)
	owner, bob, ann, eve := e.User("owner"), e.User("bob"), e.User("ann"), e.User("eve")
	gid, text, _ := e.server(owner, bob, ann)
	g := "/api/v1/guilds/" + gid
	plain := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "тихая"}))["id"])
	open := obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "игроки", "mentionable": true}))
	if open["mentionable"] != true {
		t.Fatalf("role = %v", open)
	}
	loud := str(open["id"])
	roleText := " <@&" + plain + "> <@&" + loud + "> <@&" + gid + "> <@&" + uuid.NewString() + ">"

	// A member pings members only, and only the mentionable role; eve is not
	// a member, @everyone's id and an unknown id are not roles to ping.
	m := send(bob, text, mention(ann)+" "+mention(eve)+" "+mention(bob)+roleText)
	if got := ids(m["mentions"]); !slices.Equal(got, []string{ann.ID.String(), bob.ID.String()}) {
		t.Fatalf("mentions = %v", got)
	}
	if got := ids(m["mention_roles"]); !slices.Equal(got, []string{loud}) {
		t.Fatalf("mention_roles = %v", got)
	}
	// With MENTION_EVERYONE (the owner has all) any role of the guild.
	m = send(owner, text, roleText)
	if got := ids(m["mention_roles"]); !slices.Equal(got, []string{plain, loud}) {
		t.Fatalf("owner mention_roles = %v", got)
	}
	// What history returns is what was stored.
	if h := obj(list(bob.Expect(http.StatusOK, "GET", msgs(text)+"?limit=1", nil))[0]); !slices.Equal(ids(h["mention_roles"]), []string{plain, loud}) {
		t.Fatalf("history = %v", h)
	}
	// Making the quiet role mentionable lets bob ping it.
	owner.Expect(http.StatusOK, "PATCH", g+"/roles/"+plain, map[string]any{"mentionable": true})
	if m := send(bob, text, "<@&"+plain+">"); !slices.Equal(ids(m["mention_roles"]), []string{plain}) {
		t.Fatalf("after PATCH = %v", m["mention_roles"])
	}
	owner.Expect(http.StatusBadRequest, "PATCH", g+"/roles/"+gid, map[string]any{"mentionable": true})

	// No mentions: empty arrays, not null.
	if m := send(bob, text, "просто"); len(list(m["mentions"])) != 0 || len(list(m["mention_roles"])) != 0 {
		t.Fatalf("empty = %v", m)
	}
	// Too many distinct mentions.
	var many strings.Builder
	for range maxMentions + 1 {
		many.WriteString("<@" + uuid.NewString() + ">")
	}
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": many.String()})

	// A reply pings the author of the replied message unless turned off,
	// and never the sender.
	q := send(ann, text, "вопрос")
	r := obj(bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "ответ", "reply_to": q["id"]}))
	if got := ids(r["mentions"]); !slices.Equal(got, []string{ann.ID.String()}) {
		t.Fatalf("reply mentions = %v", got)
	}
	quiet := obj(bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "тихо", "reply_to": q["id"], "reply_mention": false}))
	if len(list(quiet["mentions"])) != 0 {
		t.Fatalf("quiet reply mentions = %v", quiet["mentions"])
	}
	self := obj(ann.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "сам себе", "reply_to": q["id"]}))
	if len(list(self["mentions"])) != 0 {
		t.Fatalf("self reply mentions = %v", self["mentions"])
	}
	// An edit checks mentions again and keeps the reply ping or its absence.
	ed := obj(bob.Expect(http.StatusOK, "PATCH", msgs(text)+"/"+str(r["id"]), map[string]any{"content": "ответ " + mention(owner)}))
	if got := ids(ed["mentions"]); !slices.Equal(got, []string{owner.ID.String(), ann.ID.String()}) {
		t.Fatalf("edited mentions = %v", got)
	}
	ed = obj(bob.Expect(http.StatusOK, "PATCH", msgs(text)+"/"+str(quiet["id"]), map[string]any{"content": "тихо " + mention(owner)}))
	if got := ids(ed["mentions"]); !slices.Equal(got, []string{owner.ID.String()}) {
		t.Fatalf("edited quiet mentions = %v", got)
	}
	// An edit after the replied message is gone pings nobody for it.
	ann.Expect(http.StatusNoContent, "DELETE", msgs(text)+"/"+str(q["id"]), nil)
	ed = obj(bob.Expect(http.StatusOK, "PATCH", msgs(text)+"/"+str(r["id"]), map[string]any{"content": "ответ"}))
	if len(list(ed["mentions"])) != 0 {
		t.Fatalf("mentions after delete = %v", ed["mentions"])
	}

	// A member who loses a role's ping right cannot ping it anymore, even by
	// editing an old message that did.
	owner.Expect(http.StatusOK, "PATCH", g+"/roles/"+loud, map[string]any{"mentionable": false})
	old := send(bob, text, "x")
	ed = obj(bob.Expect(http.StatusOK, "PATCH", msgs(text)+"/"+str(old["id"]), map[string]any{"content": "<@&" + loud + ">"}))
	if len(list(ed["mention_roles"])) != 0 {
		t.Fatalf("mention_roles after the role became quiet = %v", ed["mention_roles"])
	}

	// MENTION_EVERYONE on @everyone lets anyone ping any role and @here.
	owner.Expect(http.StatusOK, "PATCH", g+"/roles/"+gid, map[string]any{"permissions": permStr(perm.DefaultEveryone | perm.MentionEveryone)})
	if m := send(bob, text, "<@&"+loud+"> @here"); !slices.Equal(ids(m["mention_roles"]), []string{loud}) || m["mention_everyone"] != true {
		t.Fatalf("with MENTION_EVERYONE = %v", m)
	}
}

func TestMentionsInDM(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")
	dm := e.dm(ann, bob)
	m := send(ann, dm, mention(bob)+" "+mention(eve)+" <@&"+uuid.NewString()+"> @everyone")
	if got := ids(m["mentions"]); !slices.Equal(got, []string{bob.ID.String()}) {
		t.Fatalf("DM mentions = %v", got)
	}
	if len(list(m["mention_roles"])) != 0 || m["mention_everyone"] != false {
		t.Fatalf("DM message = %v", m)
	}
}
