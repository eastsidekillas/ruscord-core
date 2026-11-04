package guild

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Mentions keeps the ids a message in guildID may ping (ADR-0035): users who
// are members of the guild, and roles of the guild that are mentionable, or
// any of its roles when pingAll (the author has MENTION_EVERYONE). @everyone
// is not a role mention: it has its own text. Both results keep the order of
// the input and are never nil.
func (s *Service) Mentions(ctx context.Context, guildID uuid.UUID, users, roles []uuid.UUID, pingAll bool) ([]uuid.UUID, []uuid.UUID, error) {
	u, err := s.keepFound(ctx, users,
		`SELECT user_id FROM members WHERE guild_id = $1 AND user_id = ANY($2)`, guildID, users)
	if err != nil {
		return nil, nil, err
	}
	r, err := s.keepFound(ctx, roles,
		`SELECT id FROM roles WHERE guild_id = $1 AND id = ANY($2) AND id <> $1 AND (mentionable OR $3)`,
		guildID, roles, pingAll)
	return u, r, err
}

// keepFound returns the ids that the query, run with args, returns.
func (s *Service) keepFound(ctx context.Context, ids []uuid.UUID, sql string, args ...any) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pg.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if slices.Contains(found, id) {
			out = append(out, id)
		}
	}
	return out, nil
}
