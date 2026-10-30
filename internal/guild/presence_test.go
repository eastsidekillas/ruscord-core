package guild

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/presence"
)

type fakePresences map[uuid.UUID]presence.Status

func (f fakePresences) Statuses(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]presence.Status, error) {
	out := make(map[uuid.UUID]presence.Status, len(ids))
	for _, id := range ids {
		st, ok := f[id]
		if !ok {
			st = presence.Offline
		}
		out[id] = st
	}
	return out, nil
}

// fill adds n generated members to a guild directly, to reach sizes that
// would take minutes through the API.
func (e *env) fill(guildID string, n int) {
	e.T.Helper()
	if _, err := e.PG.Exec(context.Background(), `
		WITH u AS (
			INSERT INTO users (id, email, username, display_name, password_hash)
			SELECT gen_random_uuid(), 'f' || i || '_' || $1 || '@example.com', 'f' || i || '_' || left($1, 8), 'f', 'x'
			FROM generate_series(1, $2) AS i
			RETURNING id)
		INSERT INTO members (guild_id, user_id) SELECT $1::uuid, id FROM u`, guildID, n); err != nil {
		e.T.Fatal(err)
	}
}

func TestMemberStatuses(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	e.svc.WithPresences(fakePresences{owner.ID: presence.DND})
	small, _, _ := createGuild(owner)
	e.join(small, bob)

	ms := list(bob.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+small+"/members", nil))
	got := map[string]any{}
	for _, m := range ms {
		got[str(obj(m)["user_id"])] = obj(m)["status"]
	}
	if got[owner.ID.String()] != "dnd" || got[bob.ID.String()] != "offline" {
		t.Fatalf("statuses in a small guild = %v", got)
	}

	big, _, _ := createGuild(owner)
	e.fill(big, presence.MaxGuildMembers) // owner + 250 > 250
	for _, m := range list(owner.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+big+"/members?limit=1000", nil)) {
		if _, ok := obj(m)["status"]; ok {
			t.Fatalf("a guild over %d members must not carry statuses: %v", presence.MaxGuildMembers, m)
		}
	}
}

func TestSmallGuilds(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	two, _, _ := createGuild(owner)
	e.join(two, bob)
	three, _, _ := createGuild(owner)
	e.join(three, bob)
	e.join(three, eve)
	createGuild(eve) // bob is not in it

	ids := func(max int) []string {
		t.Helper()
		gs, err := e.svc.SmallGuilds(t.Context(), bob.ID, max)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(gs))
		for i, g := range gs {
			out[i] = g.String()
		}
		slices.Sort(out)
		return out
	}
	both := []string{two, three}
	slices.Sort(both)
	if got := ids(3); !slices.Equal(got, both) {
		t.Fatalf("max 3: %v, want both of bob's guilds", got)
	}
	if got := ids(2); !slices.Equal(got, []string{two}) {
		t.Fatalf("max 2: %v, want only the two-member guild", got)
	}
	if got := ids(1); len(got) != 0 {
		t.Fatalf("max 1: %v", got)
	}
}
