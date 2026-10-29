// Package store opens connections to the shared infrastructure:
// Postgres (source of truth), Redis/Valkey (ephemeral state) and NATS (events).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

type Clients struct {
	PG    *pgxpool.Pool
	Redis *redis.Client
	NATS  *nats.Conn
}

type Options struct {
	PostgresURL   string
	RedisAddr     string
	RedisPassword string
	NATSURL       string
	ClientName    string
}

// Open connects to all backing services. Connections are lazy where the
// driver allows it; readiness is checked separately via the health package.
func Open(ctx context.Context, o Options) (*Clients, error) {
	pg, err := pgxpool.New(ctx, o.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
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
		_ = rdb.Close()
		return nil, fmt.Errorf("nats: %w", err)
	}

	return &Clients{PG: pg, Redis: rdb, NATS: nc}, nil
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
}
