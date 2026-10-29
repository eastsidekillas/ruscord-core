package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream settings. Subjects are the outbox topics. Gateways resume from a
// JetStream sequence, so MaxAge bounds how long a client may be offline and
// still RESUME instead of reloading everything.
const (
	StreamName = "EVENTS"
	streamAge  = 24 * time.Hour
	// dupWindow must outlast the time between a publish and the commit that
	// marks the row published, so a relay crash in between does not duplicate.
	dupWindow = 2 * time.Minute

	// HeaderType carries the event type (DISPATCH.t) next to the JSON payload.
	HeaderType = "Ruscord-Event-Type"

	notifyChannel = "outbox"
	// relayLockKey is the advisory lock that keeps one relay active per database.
	relayLockKey = 0x72757363_6f726401 // "ruscord" + 1
)

var streamSubjects = []string{"guild.>", "user.>", "channel.>"}

// EnsureStream creates or updates the events stream.
func EnsureStream(ctx context.Context, js jetstream.JetStream) (jetstream.Stream, error) {
	return js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamName,
		Subjects:   streamSubjects,
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		MaxAge:     streamAge,
		Duplicates: dupWindow,
		Discard:    jetstream.DiscardOld,
	})
}

// Relay publishes outbox rows to JetStream in id order.
//
// Ordering: one relay holds an advisory lock per batch, so rows go out in
// commit-visible id order even with several API processes. Delivery: a row
// is marked published only after JetStream acknowledged it; the outbox id is
// the Nats-Msg-Id, so a retry after a crash is deduplicated by the stream.
type Relay struct {
	pg    *pgxpool.Pool
	js    jetstream.JetStream
	log   *slog.Logger
	batch int
	// idle is the fallback poll interval when no NOTIFY arrives.
	idle time.Duration
	// keep is how long published rows stay in the table, for debugging.
	keep time.Duration
}

func NewRelay(pg *pgxpool.Pool, nc *nats.Conn, log *slog.Logger) (*Relay, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	return &Relay{pg: pg, js: js, log: log, batch: 256, idle: time.Second, keep: time.Hour}, nil
}

// Run publishes until ctx is cancelled. Errors are logged and retried with
// backoff; Run only returns when ctx is done.
func (r *Relay) Run(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		err := r.loop(ctx)
		if ctx.Err() != nil {
			return
		}
		r.log.Error("outbox relay stopped, restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (r *Relay) loop(ctx context.Context) error {
	if _, err := EnsureStream(ctx, r.js); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}
	conn, err := r.pg.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	lastClean := time.Time{}
	for {
		n, err := r.PublishBatch(ctx)
		if err != nil {
			return err
		}
		if time.Since(lastClean) > time.Minute {
			if err := r.clean(ctx); err != nil {
				return err
			}
			lastClean = time.Now()
		}
		if n == r.batch {
			continue // more is waiting
		}
		wctx, cancel := context.WithTimeout(ctx, r.idle)
		_, err = conn.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
}

type row struct {
	id      int64
	topic   string
	typ     string
	payload []byte
}

// PublishBatch publishes up to one batch and reports how many rows it sent.
// It sends nothing when another relay holds the lock.
func (r *Relay) PublishBatch(ctx context.Context) (int, error) {
	var sent int
	err := pgx.BeginFunc(ctx, r.pg, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(relayLockKey)).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return nil
		}
		rows, err := tx.Query(ctx,
			`SELECT id, topic, type, payload FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT $1`, r.batch)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
			var x row
			err := cr.Scan(&x.id, &x.topic, &x.typ, &x.payload)
			return x, err
		})
		if err != nil || len(batch) == 0 {
			return err
		}
		// Publish in order and wait for each ack, so a failure leaves a
		// prefix published and the rest for the next attempt.
		ids := make([]int64, 0, len(batch))
		for _, x := range batch {
			msg := &nats.Msg{Subject: x.topic, Data: x.payload, Header: nats.Header{}}
			msg.Header.Set(HeaderType, x.typ)
			if _, err := r.js.PublishMsg(ctx, msg, jetstream.WithMsgID(strconv.FormatInt(x.id, 10))); err != nil {
				if len(ids) == 0 {
					return fmt.Errorf("publish %d: %w", x.id, err)
				}
				r.log.Warn("outbox publish failed mid-batch", "id", x.id, "err", err)
				break
			}
			ids = append(ids, x.id)
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
		sent = len(ids)
		return nil
	})
	return sent, err
}

func (r *Relay) clean(ctx context.Context) error {
	_, err := r.pg.Exec(ctx, `DELETE FROM outbox WHERE published_at < now() - $1::interval`,
		strconv.Itoa(int(r.keep/time.Second))+" seconds")
	return err
}
