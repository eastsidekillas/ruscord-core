package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// revokedTTL covers the life of the last access token of a revoked
// session: accessTTL plus the leeway Verify allows.
const revokedTTL = accessTTL + time.Minute

// Revocations makes a revoked sign-in session unusable at once. Access
// tokens are verified without the database, so a revoked session is listed
// in Valkey for as long as its access tokens live, and RequireAuth checks
// the list (docs/adr/0023-session-management.md).
type Revocations struct {
	rdb *redis.Client
}

func NewRevocations(rdb *redis.Client) *Revocations { return &Revocations{rdb: rdb} }

func revokedKey(id uuid.UUID) string { return "revoked_session:" + id.String() }

// Mark lists sessions as revoked. It runs after the revocation is committed:
// listing a session whose revocation then rolled back would lock its device
// out for revokedTTL.
func (r *Revocations) Mark(ctx context.Context, ids ...uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	pipe := r.rdb.Pipeline()
	for _, id := range ids {
		pipe.Set(ctx, revokedKey(id), 1, revokedTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	return nil
}

// Revoked reports whether the session was revoked within revokedTTL.
func (r *Revocations) Revoked(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := r.rdb.Exists(ctx, revokedKey(id)).Result()
	if err != nil {
		return false, fmt.Errorf("revocations: %w", err)
	}
	return n > 0, nil
}
