// Package ratelimit implements fixed-window rate limits on Valkey.
package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

type Limiter struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Limiter { return &Limiter{rdb: rdb} }

// Rule allows Limit events per Window for one key.
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// Allow records one event for key under rule. It returns whether the event
// is within the limit and, if not, how long until the window resets.
func (l *Limiter) Allow(ctx context.Context, rule Rule, key string) (bool, time.Duration, error) {
	k := fmt.Sprintf("ratelimit:%s:%s", rule.Name, key)
	// SET NX EX first guarantees the key always carries a TTL, even if the
	// process dies between the two commands.
	pipe := l.rdb.TxPipeline()
	pipe.SetNX(ctx, k, 0, rule.Window)
	incr := pipe.Incr(ctx, k)
	ttl := pipe.PTTL(ctx, k)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	if incr.Val() > rule.Limit {
		return false, ttl.Val(), nil
	}
	return true, 0, nil
}

// Check is Allow for request handlers: it returns a 429 API error when the
// limit is exceeded.
func (l *Limiter) Check(ctx context.Context, rule Rule, key string) error {
	ok, retry, err := l.Allow(ctx, rule, key)
	if err != nil {
		return err
	}
	if !ok {
		secs := int(retry.Round(time.Second) / time.Second)
		return httpx.NewError(http.StatusTooManyRequests, "rate_limited",
			fmt.Sprintf("Слишком много попыток, повторите через %d с", max(secs, 1)))
	}
	return nil
}
