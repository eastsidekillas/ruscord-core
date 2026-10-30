package social

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// Profile is what a user's profile card shows.
type Profile struct {
	User auth.PublicUser `json:"user"`
	Bio  string          `json:"bio"`
}

// Profile returns another user's profile if the caller is connected to them:
// a common guild or DM, or a relationship. Like in Discord, the caller's own
// block does not hide the profile from the caller; the other side's block is
// not counted as a connection, so it is not revealed (ADR-0014). Anyone else
// gets 404, the same as for a user that does not exist.
func (s *Service) Profile(ctx context.Context, me, other uuid.UUID) (Profile, error) {
	ok, err := s.connected(ctx, me, other)
	if err != nil {
		return Profile{}, err
	}
	if !ok {
		return Profile{}, httpx.ErrNotFound
	}
	users, err := s.users.Users(ctx, []uuid.UUID{other})
	if err != nil {
		return Profile{}, err
	}
	u, found := users[other]
	if !found {
		return Profile{}, httpx.ErrNotFound
	}
	bio, err := s.users.Bio(ctx, other)
	if err != nil {
		return Profile{}, err
	}
	return Profile{User: u, Bio: bio}, nil
}

func (s *Service) connected(ctx context.Context, me, other uuid.UUID) (bool, error) {
	if me == other {
		return true, nil
	}
	var ok bool
	err := s.pg.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM relationships WHERE user_id = $1 AND target_id = $2)
		     OR EXISTS (SELECT 1 FROM relationships WHERE user_id = $2 AND target_id = $1 AND type <> $3)
		     OR EXISTS (SELECT 1 FROM dm_recipients x JOIN dm_recipients y ON y.channel_id = x.channel_id
		                WHERE x.user_id = $1 AND y.user_id = $2)`, me, other, RelBlocked).Scan(&ok)
	if err != nil || ok {
		return ok, err
	}
	return s.guilds.SharesGuild(ctx, me, other)
}

func (h *Handler) profile(w http.ResponseWriter, r *http.Request) error {
	id := auth.UserID(r)
	if r.PathValue("user") != "@me" {
		var err error
		if id, err = httpx.PathUUID(r, "user"); err != nil {
			return err
		}
	}
	p, err := h.svc.Profile(r.Context(), auth.UserID(r), id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}
