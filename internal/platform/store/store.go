// Package store opens connections to the shared infrastructure:
// Postgres (source of truth), Redis/Valkey (ephemeral state) and NATS (events).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// DB is what both a pool and a transaction offer. Code that may run inside
// another module's transaction takes one, so it reads through that
// transaction instead of waiting for a second connection.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lookupConns is the size of the Lookup pool: its queries are short reads
// by primary key, so a few connections serve many callers.
const lookupConns = 4

type Clients struct {
	PG *pgxpool.Pool
	// Lookup is a small pool for reads that other modules make inside
	// their transactions (auth.Deps.Lookup).
	Lookup *pgxpool.Pool
	Redis  *redis.Client
	NATS   *nats.Conn
}

type Options struct {
	PostgresURL string
	// PGMaxConns caps the Postgres pool; 0 keeps the driver default
	// (pool_max_conns in the URL, else max(4, NumCPU)).
	PGMaxConns    int32
	RedisAddr     string
	RedisPassword string
	NATSURL       string
	ClientName    string
}

// Open connects to all backing services. Connections are lazy where the
// driver allows it; readiness is checked separately via the health package.
func Open(ctx context.Context, o Options) (*Clients, error) {
	cfg, err := pgxpool.ParseConfig(o.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if o.PGMaxConns > 0 {
		cfg.MaxConns = o.PGMaxConns
	}
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	lcfg := cfg.Copy()
	lcfg.MaxConns, lcfg.MinConns = lookupConns, 0
	lookup, err := pgxpool.NewWithConfig(ctx, lcfg)
	if err != nil {
		pg.Close()
		return nil, fmt.Errorf("postgres lookup: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:       o.RedisAddr,
		Password:   o.RedisPassword,
		ClientName: o.ClientName,
	})

	nc, err := nats.Connect(o.NATSURL,
		nats.Name(o.ClientName),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		pg.Close()
		lookup.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("nats: %w", err)
	}

	return &Clients{PG: pg, Lookup: lookup, Redis: rdb, NATS: nc}, nil
}

func (c *Clients) Close() {
	if c.NATS != nil {
		_ = c.NATS.Drain()
	}
	if c.Redis != nil {
		_ = c.Redis.Close()
	}
	if c.PG != nil {
		c.PG.Close()
	}
	if c.Lookup != nil {
		c.Lookup.Close()
	}
}
