package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// AccountCleanup takes a deleted account out of what another module keeps:
// guilds, groups, friends. It must be idempotent: the sweeper calls it again
// until a pass succeeds, and once more after access tokens of the account
// have expired, in case they were used to join something meanwhile.
type AccountCleanup interface {
	CleanupAccount(ctx context.Context, user uuid.UUID) error
}

// OnAccountDeleted registers the modules that clean up after a deletion.
func (s *Service) OnAccountDeleted(c ...AccountCleanup) { s.cleanups = append(s.cleanups, c...) }

var errOwnsGuilds = httpx.NewError(http.StatusBadRequest, "owns_guilds",
	"Сначала удалите серверы, владельцем которых вы являетесь")

// cleanupAgain is how long after a deletion the last cleanup pass runs: by
// then every access token of the account has expired (accessTTL).
const cleanupAgain = accessTTL + 5*time.Minute

type DeleteMeInput struct {
	Password string `json:"password"`
}

// DeleteMe deletes the caller's account. The password is asked again, like
// for any irreversible action. The account is hidden and unusable at once:
// sign-in fails, every session is revoked (gateway connections close), others
// see "deleted user". Its rows stay; nothing is shown from them
// (docs/adr/0021-bans-account-deletion.md). Leaving guilds, groups and
// friends follows right after, and the sweeper finishes it if that fails.
func (s *Service) DeleteMe(ctx context.Context, p Principal, in DeleteMeInput) error {
	if _, err := s.checkPassword(ctx, p.UserID, in.Password); err != nil {
		return err
	}
	if s.guilds != nil {
		n, err := s.guilds.OwnedGuildCount(ctx, p.UserID)
		if err != nil {
			return err
		}
		if n > 0 {
			return errOwnsGuilds
		}
	}
	now := s.now().UTC()
	var revoked []uuid.UUID
	err := pgx.BeginFunc(ctx, s.store.pg, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET deleted_at = $2 WHERE id = $1 AND deleted_at IS NULL`, p.UserID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.ErrUnauthorized // deleted meanwhile
		}
		rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, p.UserID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		// Revoking closes the gateway connections of every device (4004);
		// the next ticket request answers 401 and the client signs out.
		for _, id := range ids {
			if err := revokeSession(ctx, tx, id, "account_deleted", now); err != nil {
				return err
			}
		}
		revoked = ids
		return nil
	})
	if err != nil {
		return err
	}
	s.markRevoked(ctx, revoked...)
	// The account is already gone for everyone; a failure here is retried.
	if err := s.cleanup(ctx, p.UserID); err != nil {
		slog.WarnContext(ctx, "auth: account cleanup failed, the sweeper will retry", "user", p.UserID, "err", err)
	}
	return nil
}

// RunAccountCleanup finishes deletions: until a pass succeeds, and once more
// after cleanupAgain. It runs on every API node; passes are idempotent.
func (s *Service) RunAccountCleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.SweepDeletedAccounts(ctx); err != nil {
			slog.WarnContext(ctx, "auth: account cleanup sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepDeletedAccounts runs one cleanup pass over accounts that need one and
// reports how many passes succeeded.
func (s *Service) SweepDeletedAccounts(ctx context.Context) (int, error) {
	rows, err := s.store.pg.Query(ctx,
		`SELECT id FROM users
		 WHERE deleted_at IS NOT NULL
		   AND (cleaned_at IS NULL OR (cleaned_at < deleted_at + $1::interval AND now() >= deleted_at + $1::interval))
		 ORDER BY deleted_at LIMIT 50`, fmt.Sprintf("%d seconds", int(cleanupAgain/time.Second)))
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	done := 0
	var errs []error
	for _, id := range ids {
		if err := s.cleanup(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		done++
	}
	return done, errors.Join(errs...)
}

func (s *Service) cleanup(ctx context.Context, user uuid.UUID) error {
	for _, c := range s.cleanups {
		if err := c.CleanupAccount(ctx, user); err != nil {
			return err
		}
	}
	_, err := s.store.pg.Exec(ctx, `UPDATE users SET cleaned_at = now() WHERE id = $1`, user)
	return err
}
