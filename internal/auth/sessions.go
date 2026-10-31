package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// maxSessions is how many devices may be signed in at once; signing in on
// one more revokes the least recently used.
const maxSessions = 100

var (
	errWrongPassword = httpx.NewError(http.StatusForbidden, "wrong_password", "Неверный пароль")

	// rulePasswordCheck limits password re-entry for sensitive actions per
	// user, so a stolen session cannot be used to guess the password.
	rulePasswordCheck = ratelimit.Rule{Name: "password_check", Limit: 5, Window: time.Hour}
)

// SessionInfo is a signed-in device as its owner sees it.
type SessionInfo struct {
	ID     uuid.UUID `json:"id"`
	Device string    `json:"device"` // the User-Agent at sign-in
	// IP is the address of the last refresh, or of the sign-in.
	IP         *string   `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Current    bool      `json:"current"`
}

const sessionInfoCols = `id, device, host(COALESCE(last_ip, ip)), created_at, last_used_at`

func scanSessionInfo(row pgx.CollectableRow) (SessionInfo, error) {
	var si SessionInfo
	err := row.Scan(&si.ID, &si.Device, &si.IP, &si.CreatedAt, &si.LastUsedAt)
	return si, err
}

func sessionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (SessionInfo, error) {
	rows, err := tx.Query(ctx, `SELECT `+sessionInfoCols+` FROM sessions WHERE id = $1`, id)
	if err != nil {
		return SessionInfo{}, err
	}
	return pgx.CollectExactlyOneRow(rows, scanSessionInfo)
}

// Sessions lists the caller's active sessions, most recently used first.
func (s *Service) Sessions(ctx context.Context, p Principal) ([]SessionInfo, error) {
	rows, err := s.store.pg.Query(ctx,
		`SELECT `+sessionInfoCols+` FROM sessions
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		 ORDER BY last_used_at DESC, id DESC`, p.UserID, s.now().UTC())
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanSessionInfo)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Current = list[i].ID == p.SessionID
	}
	return list, nil
}

// RevokeSession signs one of the caller's devices out (the current one too).
// Someone else's session and an inactive one are both 404.
func (s *Service) RevokeSession(ctx context.Context, p Principal, id uuid.UUID) error {
	now := s.now().UTC()
	err := s.store.withSessionLocked(ctx, id, func(tx pgx.Tx, sess session) error {
		if sess.UserID != p.UserID || sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) {
			return errNotFound
		}
		return revokeSession(ctx, tx, id, "revoked_by_user", now)
	})
	if errors.Is(err, errNotFound) {
		return httpx.ErrNotFound
	}
	if err != nil {
		return err
	}
	s.markRevoked(ctx, id)
	return nil
}

// PasswordInput confirms a sensitive action with the account password.
type PasswordInput struct {
	Password string `json:"password"`
}

// RevokeOtherSessions signs out every device but the caller's. It asks for
// the password: whoever holds an open tab for a minute must not be able to
// lock the owner out everywhere.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal, in PasswordInput) (int, error) {
	if _, err := s.checkPassword(ctx, p.UserID, in.Password); err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	err := pgx.BeginFunc(ctx, s.store.pg, func(tx pgx.Tx) error {
		var err error
		ids, err = revokeOthers(ctx, tx, p, "revoked_by_user", s.now().UTC())
		return err
	})
	if err != nil {
		return 0, err
	}
	s.markRevoked(ctx, ids...)
	return len(ids), nil
}

type ChangePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword sets a new password and signs out every other device: a
// password is changed because someone else may know it.
func (s *Service) ChangePassword(ctx context.Context, p Principal, in ChangePasswordInput) error {
	u, err := s.checkPassword(ctx, p.UserID, in.CurrentPassword)
	if err != nil {
		return err
	}
	if msg := passwordProblem(in.NewPassword, u.Email, u.Username); msg != "" {
		return httpx.Validation(map[string]string{"new_password": msg})
	}
	hash, err := s.hasher.Hash(ctx, in.NewPassword)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	err = pgx.BeginFunc(ctx, s.store.pg, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, p.UserID, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.ErrUnauthorized
		}
		ids, err = revokeOthers(ctx, tx, p, "password_changed", s.now().UTC())
		return err
	})
	if err != nil {
		return err
	}
	s.markRevoked(ctx, ids...)
	return nil
}

// revokeOthers revokes the active sessions of p's user except p's own.
func revokeOthers(ctx context.Context, tx pgx.Tx, p Principal, reason string, now time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM sessions WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL AND expires_at > $3 ORDER BY id`,
		p.UserID, p.SessionID, now)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := revokeSession(ctx, tx, id, reason, now); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// checkPassword confirms a sensitive action: the account's password, asked
// again, at most rulePasswordCheck times per user.
func (s *Service) checkPassword(ctx context.Context, user uuid.UUID, password string) (User, error) {
	if err := s.limit(ctx, rulePasswordCheck, user.String()); err != nil {
		return User{}, err
	}
	u, err := s.store.userByID(ctx, user)
	if errors.Is(err, errNotFound) {
		return User{}, httpx.ErrUnauthorized
	}
	if err != nil {
		return User{}, err
	}
	if len(password) > 512 {
		return User{}, errWrongPassword
	}
	ok, err := s.hasher.Verify(ctx, password, u.passwordHash)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, errWrongPassword
	}
	return u, nil
}
