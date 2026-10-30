package events

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eastsidekillas/ruscord-core/internal/platform/natstest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
)

// Needs RUSCORD_TEST_POSTGRES_URL; NATS runs in-process.

func write(t *testing.T, pg *pgxpool.Pool, evs ...Event) {
	t.Helper()
	if err := pgx.BeginFunc(t.Context(), pg, func(tx pgx.Tx) error { return Write(t.Context(), tx, evs...) }); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*pgxpool.Pool, *Relay, jetstream.Stream) {
	pg, nc := testdb.New(t), natstest.New(t)
	r, err := NewRelay(pg, nc, testdb.Logger())
	if err != nil {
		t.Fatal(err)
	}
	st, err := EnsureStream(t.Context(), r.js)
	if err != nil {
		t.Fatal(err)
	}
	return pg, r, st
}

func messages(t *testing.T, st jetstream.Stream) []jetstream.RawStreamMsg {
	t.Helper()
	info, err := st.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []jetstream.RawStreamMsg
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		m, err := st.GetMsg(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *m)
	}
	return out
}

func TestRelayPublishesInOrderOnce(t *testing.T) {
	pg, r, st := setup(t)
	g, u := uuid.New(), uuid.New()
	write(t, pg,
		Event{Topic: GuildTopic(g), Type: GuildUpdate, Data: map[string]int{"n": 1}},
		Event{Topic: UserTopic(u), Type: GuildCreate, Data: map[string]int{"n": 2}},
	)
	write(t, pg, Event{Topic: GuildTopic(g), Type: ChannelCreate, Data: map[string]int{"n": 3}})

	n, err := r.PublishBatch(t.Context())
	if err != nil || n != 3 {
		t.Fatalf("PublishBatch = %d, %v", n, err)
	}
	msgs := messages(t, st)
	if len(msgs) != 3 {
		t.Fatalf("stream has %d messages", len(msgs))
	}
	want := []struct{ subj, typ, data string }{
		{GuildTopic(g), GuildUpdate, `{"n": 1}`},
		{UserTopic(u), GuildCreate, `{"n": 2}`},
		{GuildTopic(g), ChannelCreate, `{"n": 3}`},
	}
	for i, w := range want {
		m := msgs[i]
		if m.Subject != w.subj || m.Header.Get(HeaderType) != w.typ || string(m.Data) != w.data {
			t.Fatalf("message %d = %s %s %s, want %v", i, m.Subject, m.Header.Get(HeaderType), m.Data, w)
		}
	}

	// Nothing left to send.
	if n, err := r.PublishBatch(t.Context()); err != nil || n != 0 {
		t.Fatalf("second batch = %d, %v", n, err)
	}

	// A crash after publishing but before marking rows leaves them unpublished;
	// the retry must not duplicate them in the stream.
	if _, err := pg.Exec(t.Context(), `UPDATE outbox SET published_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.PublishBatch(t.Context()); err != nil || n != 3 {
		t.Fatalf("retry batch = %d, %v", n, err)
	}
	if got := len(messages(t, st)); got != 3 {
		t.Fatalf("after retry stream has %d messages, want 3", got)
	}
}

func TestRelaySkipsWhileAnotherHoldsTheLock(t *testing.T) {
	pg, r, _ := setup(t)
	write(t, pg, Event{Topic: UserTopic(uuid.New()), Type: GuildCreate, Data: 1})

	tx, err := pg.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, int64(relayLockKey)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.PublishBatch(t.Context()); err != nil || n != 0 {
		t.Fatalf("PublishBatch under foreign lock = %d, %v", n, err)
	}
	_ = tx.Rollback(t.Context())
	if n, err := r.PublishBatch(t.Context()); err != nil || n != 1 {
		t.Fatalf("PublishBatch after unlock = %d, %v", n, err)
	}
}

func TestRelayRunWakesOnNotify(t *testing.T) {
	pg, r, st := setup(t)
	r.idle = time.Hour // only NOTIFY can wake it in time
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	cons, err := st.OrderedConsumer(t.Context(), jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the relay reach LISTEN
	write(t, pg, Event{Topic: GuildTopic(uuid.New()), Type: GuildDelete, Data: map[string]string{"id": "x"}})

	msg, err := cons.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatalf("no event within 3s: %v", err)
	}
	if msg.Headers().Get(HeaderType) != GuildDelete {
		t.Fatalf("type = %q", msg.Headers().Get(HeaderType))
	}
}
