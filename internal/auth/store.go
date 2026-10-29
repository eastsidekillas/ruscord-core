package auth

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"display_name"`
	CreatedAt    time.Time `json:"created_at"`
	passwordHash string
}

type session struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	RefreshHash     []byte
	PrevRefreshHash []byte
	RotatedAt       time.Time
	ExpiresAt       time.Time
	RevokedAt       *time.Time
}

var (
	errNotFound      = errors.New("auth: not found")
	errEmailTaken    = errors.New("auth: email taken")
	errUsernameTaken = errors.New("auth: username taken")
)

// store is the only code that touches the users and sessions tables.
type store struct {
	pg *pgxpool.Pool
}

const uniqueViolation = "23505"

func (s *store) createUserWithSession(ctx context.Context, u User, sess session, device string, ip netip.Addr) error {
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, username, display_name, password_hash, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $6)`,
			u.ID, u.Email, u.Username, u.DisplayName, u.passwordHash, u.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				switch pgErr.ConstraintName {
				case "users_email_key":
					return errEmailTaken
				case "users_username_key":
					return errUsernameTaken
				}
			}
			return err
		}
		return insertSession(ctx, tx, sess, device, ip)
	})
}

func (s *store) createSession(ctx context.Context, sess session, device string, ip netip.Addr) error {
	return insertSession(ctx, s.pg, sess, device, ip)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertSession(ctx context.Context, db execer, sess session, device string, ip netip.Addr) error {
	var ipArg any
	if ip.IsValid() {
		ipArg = ip
	}
	_, err := db.Exec(ctx,
		`INSERT INTO sessions (id, user_id, refresh_hash, rotated_at, device, ip, created_at, last_used_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $4, $4, $7)`,
		sess.ID, sess.UserID, sess.RefreshHash, sess.RotatedAt, device, ipArg, sess.ExpiresAt)
	return err
}

func (s *store) userByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.pg.QueryRow(ctx,
		`SELECT id, email, username, display_name, created_at, password_hash FROM users WHERE email = $1`, email))
}

func (s *store) userByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(s.pg.QueryRow(ctx,
		`SELECT id, email, username, display_name, created_at, password_hash FROM users WHERE id = $1`, id))
}

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.CreatedAt, &u.passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, errNotFound
	}
	return u, err
}

// withSessionLocked loads a session FOR UPDATE and runs fn inside the same
// transaction, so concurrent refreshes of one session are serialised.
func (s *store) withSessionLocked(ctx context.Context, id uuid.UUID, fn func(tx pgx.Tx, sess session) error) error {
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		var sess session
		err := tx.QueryRow(ctx,
			`SELECT id, user_id, refresh_hash, prev_refresh_hash, rotated_at, expires_at, revoked_at
			 FROM sessions WHERE id = $1 FOR UPDATE`, id).
			Scan(&sess.ID, &sess.UserID, &sess.RefreshHash, &sess.PrevRefreshHash, &sess.RotatedAt, &sess.ExpiresAt, &sess.RevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return fn(tx, sess)
	})
}

func rotateSession(ctx context.Context, tx pgx.Tx, id uuid.UUID, newHash, prevHash []byte, now, expires time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET refresh_hash = $2, prev_refresh_hash = $3, rotated_at = $4, last_used_at = $4, expires_at = $5
		 WHERE id = $1`, id, newHash, prevHash, now, expires)
	return err
}

func revokeSession(ctx context.Context, db execer, id uuid.UUID, reason string, now time.Time) error {
	_, err := db.Exec(ctx,
		`UPDATE sessions SET revoked_at = $2, revoke_reason = $3 WHERE id = $1 AND revoked_at IS NULL`, id, now, reason)
	return err
}
