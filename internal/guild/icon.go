package guild

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/imaging"
)

// WithFiles enables guild icons and registers their reference check with
// the collector. Without it icon uploads answer 503.
func (s *Service) WithFiles(c *blob.Collector) {
	s.files = c
	c.Register(blob.KindIcon, s.iconInUse)
}

// SetIcon makes an uploaded picture the guild's icon; needs MANAGE_GUILD.
func (s *Service) SetIcon(ctx context.Context, userID, guildID uuid.UUID, upload []byte) (Guild, error) {
	// Check the permission before the expensive part, and again under the
	// lock when the icon is set.
	if err := s.mutate(ctx, guildID, userID, func(_ pgx.Tx, a access) error { return canManage(a) }); err != nil {
		return Guild{}, err
	}
	if err := s.limiter.Check(ctx, auth.RuleImageUpload, userID.String()); err != nil {
		return Guild{}, err
	}
	key, err := imaging.Store(ctx, s.files, blob.KindIcon, guildID, upload)
	if err != nil {
		return Guild{}, err
	}
	return s.setIcon(ctx, userID, guildID, &key)
}

// DeleteIcon removes the guild's icon; needs MANAGE_GUILD.
func (s *Service) DeleteIcon(ctx context.Context, userID, guildID uuid.UUID) (Guild, error) {
	return s.setIcon(ctx, userID, guildID, nil)
}

func (s *Service) setIcon(ctx context.Context, userID, guildID uuid.UUID, key *string) (Guild, error) {
	var g Guild
	err := s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if err := canManage(a); err != nil {
			return err
		}
		g = a.guild
		if blob.SameKey(g.iconKey, key) {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE guilds SET icon = $2 WHERE id = $1`, guildID, key); err != nil {
			return err
		}
		if g.iconKey != nil {
			if err := blob.Schedule(ctx, tx, *g.iconKey, blob.ReplacedGrace); err != nil {
				return err
			}
		}
		g.iconKey, g.Icon = key, blob.URLPtr(key)
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(g.ID), Type: events.GuildUpdate, Data: g})
	})
	return g, err
}

func canManage(a access) error {
	if !a.base().Has(perm.ManageGuild) {
		return httpx.ErrForbidden
	}
	return nil
}

// iconInUse is the blob collector's reference check for guild icons.
func (s *Service) iconInUse(ctx context.Context, key string) (bool, error) {
	var used bool
	err := s.pg.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM guilds WHERE icon = $1)`, key).Scan(&used)
	return used, err
}
