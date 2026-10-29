package auth

import (
	"context"

	"github.com/google/uuid"
)

// PublicUser is what other users may see about an account.
type PublicUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

// Users returns public profiles by id. Other modules use this instead of
// reading the users table directly. Unknown ids are absent from the map.
func (s *Service) Users(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PublicUser, error) {
	out := make(map[uuid.UUID]PublicUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.pg.Query(ctx,
		`SELECT id, username, display_name FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u PublicUser
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName); err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}
