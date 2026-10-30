package social

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// CleanupAccount takes a deleted account out of friendships, requests, its
// own blocks and group DMs, with the usual events to the other side. 1:1
// DMs stay, like in Discord: the history remains, the partner sees a deleted
// user. Others' blocks of the account stay too. Idempotent.
func (s *Service) CleanupAccount(ctx context.Context, user uuid.UUID) error {
	rows, err := s.pg.Query(ctx, `SELECT target_id FROM relationships WHERE user_id = $1`, user)
	if err != nil {
		return err
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, other := range others {
		if err := s.Remove(ctx, user, other); err != nil && !errors.Is(err, httpx.ErrNotFound) {
			return err
		}
	}
	rows, err = s.pg.Query(ctx,
		`SELECT c.id FROM dm_channels c JOIN dm_recipients r ON r.channel_id = c.id
		 WHERE r.user_id = $1 AND c.type = 3`, user)
	if err != nil {
		return err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range groups {
		if err := s.RemoveRecipient(ctx, user, id, user); err != nil && !errors.Is(err, httpx.ErrNotFound) {
			return err
		}
	}
	return nil
}
