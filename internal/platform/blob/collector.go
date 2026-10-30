package blob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefCheck reports whether some row still refers to key. Each module that
// stores keys registers one for its kind, so the collector never reads
// another module's tables.
type RefCheck func(ctx context.Context, key string) (bool, error)

// How long an object stays after it stops being used, and how long a fresh
// upload may wait for the transaction that refers to it.
const (
	ReplacedGrace = 24 * time.Hour // screens that already show the old file keep working
	UploadGrace   = time.Hour
	// lease is how long a sweep owns a row it took, so concurrent sweeps in
	// several API processes do not handle it twice.
	lease = 10 * time.Minute
)

// Collector deletes objects nothing refers to. Every key that may become
// garbage is scheduled in blob_garbage: a fresh upload before it is
// referenced (if the referring transaction fails, the object is an orphan),
// and a replaced or removed file. When a row is due, the object is deleted
// only if no module still refers to its key.
type Collector struct {
	pg    *pgxpool.Pool
	store Store
	log   *slog.Logger

	mu     sync.Mutex
	checks map[string]RefCheck // by kind
}

func NewCollector(pg *pgxpool.Pool, store Store, log *slog.Logger) *Collector {
	return &Collector{pg: pg, store: store, log: log, checks: map[string]RefCheck{}}
}

// Register sets the reference check for keys of kind.
func (c *Collector) Register(kind string, check RefCheck) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[kind] = check
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Schedule marks key as possibly garbage after grace, using q (a pool or the
// transaction that releases the key). A later schedule of the same key
// replaces the earlier one and cancels a sweep's lease on it: the key is in
// use again (the same content uploaded anew), so that sweep must not delete
// it. If the sweep already holds the row, Schedule waits for it, and the
// upload that follows writes the object again.
func Schedule(ctx context.Context, q execer, key string, grace time.Duration) error {
	_, err := q.Exec(ctx,
		`INSERT INTO blob_garbage (key, delete_after) VALUES ($1, now() + $2::interval)
		 ON CONFLICT (key) DO UPDATE SET delete_after = EXCLUDED.delete_after, lease = NULL`,
		key, fmt.Sprintf("%d seconds", int(grace/time.Second)))
	if err != nil {
		return fmt.Errorf("blob: schedule %s: %w", key, err)
	}
	return nil
}

// Upload stores a new object safely: it is scheduled first, so if the
// transaction that should refer to it never commits, the collector removes
// it; if the transaction commits, the reference keeps it.
func (c *Collector) Upload(ctx context.Context, key string, body []byte) error {
	if err := Schedule(ctx, c.pg, key, UploadGrace); err != nil {
		return err
	}
	return c.store.Put(ctx, key, body, ContentType(key[strings.LastIndexByte(key, '.')+1:]))
}

// Run sweeps every interval until ctx is done.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := c.Sweep(ctx); err != nil {
			c.log.Warn("blob: sweep", "err", err)
		} else if n > 0 {
			c.log.Info("blob: swept", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep handles up to one batch of due rows and reports how many.
func (c *Collector) Sweep(ctx context.Context) (int, error) {
	rows, err := c.pg.Query(ctx,
		`UPDATE blob_garbage SET delete_after = now() + $1::interval, lease = gen_random_uuid()
		 WHERE key IN (SELECT key FROM blob_garbage WHERE delete_after <= now()
		               ORDER BY delete_after LIMIT 100 FOR UPDATE SKIP LOCKED)
		 RETURNING key, lease`, fmt.Sprintf("%d seconds", int(lease/time.Second)))
	if err != nil {
		return 0, fmt.Errorf("blob: take due rows: %w", err)
	}
	type due struct {
		key   string
		lease [16]byte
	}
	taken, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.key, &d.lease)
		return d, err
	})
	if err != nil {
		return 0, err
	}
	for _, d := range taken {
		if err := c.collect(ctx, d.key, d.lease); err != nil {
			// The row keeps its lease and is retried when it runs out.
			c.log.Warn("blob: collect", "key", d.key, "err", err)
		}
	}
	return len(taken), nil
}

// collect deletes the object of key if nothing refers to it, holding the
// garbage row locked so a concurrent upload of the same content waits.
func (c *Collector) collect(ctx context.Context, key string, lease [16]byte) error {
	kind, _, _ := strings.Cut(key, "/")
	c.mu.Lock()
	check := c.checks[kind]
	c.mu.Unlock()
	if check == nil {
		// Unknown kind: never delete what nobody vouched for.
		return fmt.Errorf("no reference check for kind %q", kind)
	}
	return pgx.BeginFunc(ctx, c.pg, func(tx pgx.Tx) error {
		var mine bool
		err := tx.QueryRow(ctx,
			`SELECT true FROM blob_garbage WHERE key = $1 AND lease = $2 FOR UPDATE`, key, lease).Scan(&mine)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // rescheduled by an upload since the sweep took it
		}
		if err != nil {
			return err
		}
		used, err := check(ctx, key)
		if err != nil {
			return err
		}
		if !used {
			if err := c.store.Delete(ctx, key); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM blob_garbage WHERE key = $1`, key)
		return err
	})
}
