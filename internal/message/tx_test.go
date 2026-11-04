package message

import (
	"errors"
	"image/color"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/hooks"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// With one connection in the pool, a transaction that asks the pool for a
// second one waits for ever (R1 of docs/audit/2026-11-04.md); requests then
// time out and the test fails. Everything here must read only through its
// transaction or the auth lookup pool.
func TestOneConnectionIsEnough(t *testing.T) {
	e := newEnvWith(t, apitest.Options{MaxConns: 1})
	owner, bob, cat := e.User("owner"), e.User("bob"), e.User("cat")

	// Guilds: create, join by invite, rename oneself (the profile goes to
	// every guild of the user).
	g := obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Сервер"}))
	text := str(obj(list(g["channels"])[0])["id"])
	code := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{}))["code"])
	bob.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
	bob.Expect(http.StatusOK, "PATCH", "/api/v1/users/@me", map[string]any{"display_name": "Боб"})

	// Messages: a file, a reply with a mention, an edit, a reaction, a pin, read marks.
	file := upload(t, owner, text, "a.png", apitest.PNG(4, 4, color.RGBA{G: 200, A: 255}))
	chat := msgs(text)
	first := str(obj(owner.Expect(http.StatusCreated, "POST", chat, map[string]any{"content": "файл", "attachments": []any{file["id"]}}))["id"])
	reply := str(obj(bob.Expect(http.StatusCreated, "POST", chat, map[string]any{
		"content": "<@" + owner.ID.String() + "> ответ", "reply_to": first,
	}))["id"])
	bob.Expect(http.StatusOK, "PATCH", chat+"/"+reply, map[string]any{"content": "<@" + owner.ID.String() + "> поправил"})
	owner.Expect(http.StatusNoContent, "PUT", react(text, reply, "👍"), nil)
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/channels/"+text+"/pins/"+reply, nil)
	owner.Expect(http.StatusNoContent, "POST", ackPath(text, reply), nil)
	bob.Expect(http.StatusNoContent, "POST", "/api/v1/guilds/"+str(g["id"])+"/ack", nil)

	// Friends, a DM and a group with a member added later.
	dm := e.dm(owner, bob)
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+dm+"/messages", map[string]any{"content": "привет"})
	e.dm(owner, cat)
	group := str(obj(owner.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels",
		map[string]any{"recipients": []uuid.UUID{bob.ID}}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/channels/"+group+"/recipients/"+cat.ID.String(), nil)

	// Deleting the channel schedules its file; the collector removes it.
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+text, nil)
	e.Sweep()
	if n := len(e.Blobs.Keys()); n != 0 {
		t.Fatalf("files left after the sweep: %d", n)
	}
}

// A message sent while its channel is being deleted waits for the deletion
// and is not written (R2 of docs/audit/2026-11-04.md).
func TestSendWaitsForChannelDelete(t *testing.T) {
	e := newEnv(t)
	owner := e.User("owner")
	_, text, _ := e.server(owner)
	ch := uuid.MustParse(text)
	ctx := t.Context()

	// A deletion in progress, locked as guild.DeleteChannel locks it.
	tx, err := e.PG.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := hooks.LockForDelete(ctx, tx, "channels", []uuid.UUID{ch}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		_, _, err := e.messages.Send(ctx, owner.ID, ch, SendInput{Content: "успею?"})
		sent <- err
	}()
	stillWaits(t, sent, "send")
	deleteChannel(t, tx, ch)
	if err := <-sent; !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("send into a deleted channel: %v", err)
	}
	if n := e.count(text); n != 0 {
		t.Fatalf("messages left: %d", n)
	}
}

// A channel deleted while a message is being sent waits for the send and
// deletes that message too.
func TestChannelDeleteWaitsForSend(t *testing.T) {
	e := newEnv(t)
	owner := e.User("owner")
	_, text, _ := e.server(owner)
	ch := uuid.MustParse(text)
	ctx := t.Context()

	// A send in progress, locked as Send locks it.
	tx, err := e.PG.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ids, err := e.guilds.LockChannels(ctx, tx, []uuid.UUID{ch}); err != nil || len(ids) != 1 {
		t.Fatalf("lock: %v %v", ids, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO messages (id, channel_id, author_id, content) VALUES ($1, $2, $3, 'в последний момент')`,
		uuid.Must(uuid.NewV7()), ch, owner.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- e.guilds.DeleteChannel(ctx, owner.ID, ch) }()
	stillWaits(t, deleted, "channel delete")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if n := e.count(text); n != 0 {
		t.Fatalf("messages left: %d", n)
	}
}

// stillWaits fails if done is ready within a short while.
func stillWaits(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s did not wait for the lock: %v", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

// deleteChannel finishes a deletion the way guild does and commits it.
func deleteChannel(t *testing.T, tx pgx.Tx, ch uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	if err := DeleteChannels(ctx, tx, []uuid.UUID{ch}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channels WHERE id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
