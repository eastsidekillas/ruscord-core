package guild

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// Snapshots keep hidden channels (the gateway needs them to notice a
// channel becoming visible), while Full built from them matches what the
// REST API shows the same member.
func TestSnapshots(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	first, _, _ := createGuild(owner)
	second, _, _ := createGuild(owner)
	e.join(second, bob)
	e.join(first, bob)
	secret := obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+first+"/channels",
		map[string]any{"name": "secret", "type": 0}))
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+str(secret["id"])+"/permissions/"+first,
		map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})

	snaps, err := e.svc.Snapshots(t.Context(), bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].Guild.ID.String() != second || snaps[1].Guild.ID.String() != first {
		t.Fatalf("snapshots out of join order: %+v", snaps)
	}
	snap := snaps[1]
	if len(snap.Channels) != 3 || snap.Member.UserID != bob.ID || snap.Member.User == nil || len(snap.Roles) != 1 {
		t.Fatalf("snapshot of first guild: %+v", snap)
	}
	view := snap.View()
	if _, ok := view.Channels[uuid.MustParse(str(secret["id"]))]; ok || len(view.Channels) != 2 {
		t.Fatalf("view channels = %v, want the two public ones", view.Channels)
	}

	got, _ := json.Marshal(snap.Full(view))
	want, _ := json.Marshal(bob.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+first, nil))
	var gotV, wantV any
	_ = json.Unmarshal(got, &gotV)
	_ = json.Unmarshal(want, &wantV)
	if string(mustJSON(gotV)) != string(mustJSON(wantV)) {
		t.Fatalf("Full from snapshot differs from REST:\n got %s\nwant %s", got, want)
	}

	// The cursor marks the newest event of the guild the snapshot contains.
	var latest int64
	if err := e.PG.QueryRow(t.Context(), `SELECT max(id) FROM outbox WHERE topic = $1`, "guild."+first).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if snap.Cursor == 0 || snap.Cursor != latest {
		t.Fatalf("cursor = %d, want the newest guild event %d", snap.Cursor, latest)
	}
	if snaps[0].Cursor >= latest {
		t.Fatalf("second guild's cursor %d must be its own, not the first guild's %d", snaps[0].Cursor, latest)
	}

	one, err := e.svc.Snapshot(t.Context(), bob.ID, uuid.MustParse(first))
	if err != nil || one.Guild.ID.String() != first || len(one.Channels) != 3 || one.Cursor != latest {
		t.Fatalf("single snapshot: %+v, %v", one, err)
	}
	stranger := e.User("stranger")
	for _, id := range []uuid.UUID{uuid.MustParse(first), uuid.New()} {
		if _, err := e.svc.Snapshot(t.Context(), stranger.ID, id); !errors.Is(err, httpx.ErrNotFound) {
			t.Fatalf("snapshot for a non-member: %v, want not found", err)
		}
	}
	if none, err := e.svc.Snapshots(t.Context(), stranger.ID); err != nil || len(none) != 0 {
		t.Fatalf("snapshots of a user without guilds: %v, %v", none, err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
