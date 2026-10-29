// Package auth owns accounts and sessions: registration, login, refresh-token
// rotation, logout, access-token verification and gateway tickets.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// refreshGrace is how long the previous refresh token stays acceptable after
// a rotation. Two tabs refreshing at the same moment is normal; a stale token
// arriving later means it was stolen and the session is revoked.
const refreshGrace = 30 * time.Second

var (
	ErrInvalidCredentials = httpx.NewError(http.StatusUnauthorized, "invalid_credentials", "Неверный email или пароль")
	ErrSessionExpired     = httpx.NewError(http.StatusUnauthorized, "session_expired", "Сессия завершена, войдите снова")
	errEmailTakenAPI      = httpx.NewError(http.StatusConflict, "email_taken", "Этот email уже зарегистрирован")
	errUsernameTakenAPI   = httpx.NewError(http.StatusConflict, "username_taken", "Это имя пользователя занято")
)

var (
	ruleLoginIP    = ratelimit.Rule{Name: "login_ip", Limit: 20, Window: time.Minute}
	ruleLoginEmail = ratelimit.Rule{Name: "login_email", Limit: 5, Window: time.Minute}
	ruleRegisterIP = ratelimit.Rule{Name: "register_ip", Limit: 5, Window: time.Hour}
	ruleRefresh    = ratelimit.Rule{Name: "refresh_session", Limit: 30, Window: time.Minute}
	ruleTicket     = ratelimit.Rule{Name: "gw_ticket", Limit: 30, Window: time.Minute}
)

type Service struct {
	store   *store
	hasher  *Hasher
	signer  *Signer
	limiter *ratelimit.Limiter
	tickets *Tickets
	now     func() time.Time
	dummy   string
}

type Deps struct {
	PG      *pgxpool.Pool
	Signer  *Signer
	Limiter *ratelimit.Limiter
	Tickets *Tickets
	Now     func() time.Time // nil means time.Now
}

func NewService(ctx context.Context, d Deps) (*Service, error) {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		store:   &store{pg: d.PG},
		hasher:  NewHasher(4),
		signer:  d.Signer,
		limiter: d.Limiter,
		tickets: d.Tickets,
		now:     now,
	}
	dummy, err := s.hasher.Hash(ctx, "ruscord-dummy-password")
	if err != nil {
		return nil, err
	}
	s.dummy = dummy
	return s, nil
}

// Tokens is what a successful sign-in hands to the transport layer.
type Tokens struct {
	Access  string
	Refresh string
	User    User
}

type ClientInfo struct {
	IP     string
	Device string
}

type RegisterInput struct {
	Email       string `json:"email"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.]{2,32}$`)

const maxEmail = 254

func (in *RegisterInput) normalize() *httpx.Error {
	in.Email = strings.TrimSpace(in.Email)
	in.Username = strings.TrimSpace(in.Username)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}

	fields := httpx.Fields{}
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email || len(in.Email) > maxEmail {
		fields["email"] = "Некорректный email"
	}
	switch {
	case !usernameRe.MatchString(in.Username):
		fields["username"] = "От 2 до 32 символов: латиница, цифры, _ и ."
	case strings.Contains(in.Username, ".."):
		fields["username"] = "Нельзя две точки подряд"
	}
	fields.Length("display_name", in.DisplayName, 1, 32)
	switch n := utf8.RuneCountInString(in.Password); {
	case n < 8:
		fields["password"] = "Не короче 8 символов"
	case n > 128:
		fields["password"] = "Не длиннее 128 символов"
	case strings.EqualFold(in.Password, in.Email) || strings.EqualFold(in.Password, in.Username):
		fields["password"] = "Пароль не должен совпадать с email или именем"
	}
	if len(fields) > 0 {
		return httpx.Validation(fields)
	}
	return nil
}

func (s *Service) limit(ctx context.Context, rule ratelimit.Rule, key string) error {
	return s.limiter.Check(ctx, rule, key)
}

func (s *Service) Register(ctx context.Context, in RegisterInput, c ClientInfo) (Tokens, error) {
	if err := s.limit(ctx, ruleRegisterIP, c.IP); err != nil {
		return Tokens{}, err
	}
	if verr := in.normalize(); verr != nil {
		return Tokens{}, verr
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return Tokens{}, err
	}
	// Postgres keeps microseconds; truncate so this answer matches later reads.
	now := s.now().UTC().Truncate(time.Microsecond)
	u := User{
		ID:           uuid.Must(uuid.NewV7()),
		Email:        in.Email,
		Username:     in.Username,
		DisplayName:  in.DisplayName,
		CreatedAt:    now,
		passwordHash: hash,
	}
	sess, refresh, err := s.newSession(u.ID, now)
	if err != nil {
		return Tokens{}, err
	}
	switch err := s.store.createUserWithSession(ctx, u, sess, c.Device, parseIP(c.IP)); {
	case errors.Is(err, errEmailTaken):
		return Tokens{}, errEmailTakenAPI
	case errors.Is(err, errUsernameTaken):
		return Tokens{}, errUsernameTakenAPI
	case err != nil:
		return Tokens{}, err
	}
	return s.tokens(u, sess.ID, refresh, now)
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Service) Login(ctx context.Context, in LoginInput, c ClientInfo) (Tokens, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if err := s.limit(ctx, ruleLoginIP, c.IP); err != nil {
		return Tokens{}, err
	}
	// An address longer than any stored one cannot match; refusing it early
	// also keeps rate-limit keys short.
	if len(email) > maxEmail {
		return Tokens{}, ErrInvalidCredentials
	}
	if err := s.limit(ctx, ruleLoginEmail, email); err != nil {
		return Tokens{}, err
	}
	if len(in.Password) > 512 {
		return Tokens{}, ErrInvalidCredentials
	}

	u, err := s.store.userByEmail(ctx, email)
	hash := u.passwordHash
	switch {
	case errors.Is(err, errNotFound):
		hash = s.dummy
	case err != nil:
		return Tokens{}, err
	}
	ok, verr := s.hasher.Verify(ctx, in.Password, hash)
	if verr != nil {
		return Tokens{}, verr
	}
	if !ok || errors.Is(err, errNotFound) {
		return Tokens{}, ErrInvalidCredentials
	}

	now := s.now().UTC()
	sess, refresh, err := s.newSession(u.ID, now)
	if err != nil {
		return Tokens{}, err
	}
	if err := s.store.createSession(ctx, sess, c.Device, parseIP(c.IP)); err != nil {
		return Tokens{}, err
	}
	return s.tokens(u, sess.ID, refresh, now)
}

// Refresh rotates the refresh token and issues a new access token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	sid, secret, err := decodeRefresh(refreshToken)
	if err != nil {
		return Tokens{}, ErrSessionExpired
	}
	if err := s.limit(ctx, ruleRefresh, sid.String()); err != nil {
		return Tokens{}, err
	}
	presented := hashSecret(secret)
	now := s.now().UTC()

	var (
		newRefresh string
		userID     uuid.UUID
		reused     bool
	)
	err = s.store.withSessionLocked(ctx, sid, func(tx pgx.Tx, sess session) error {
		if sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) {
			return ErrSessionExpired
		}
		current := subtle.ConstantTimeCompare(presented, sess.RefreshHash) == 1
		previous := sess.PrevRefreshHash != nil &&
			subtle.ConstantTimeCompare(presented, sess.PrevRefreshHash) == 1 &&
			now.Sub(sess.RotatedAt) < refreshGrace
		if !current && !previous {
			reused = true
			return revokeSession(ctx, tx, sess.ID, "refresh_reuse", now)
		}
		next, err := newRefreshSecret()
		if err != nil {
			return err
		}
		if err := rotateSession(ctx, tx, sess.ID, hashSecret(next), sess.RefreshHash, now, now.Add(refreshTTL)); err != nil {
			return err
		}
		newRefresh = encodeRefresh(sess.ID, next)
		userID = sess.UserID
		return nil
	})
	switch {
	case errors.Is(err, errNotFound):
		return Tokens{}, ErrSessionExpired
	case err != nil:
		return Tokens{}, err
	case reused:
		slog.WarnContext(ctx, "refresh token reuse detected, session revoked", "session_id", sid)
		return Tokens{}, ErrSessionExpired
	}

	u, err := s.store.userByID(ctx, userID)
	if err != nil {
		return Tokens{}, err
	}
	return s.tokens(u, sid, newRefresh, now)
}

// Logout revokes the session the refresh token belongs to. It never fails
// for a bad or unknown token: the caller is signed out either way.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	sid, secret, err := decodeRefresh(refreshToken)
	if err != nil {
		return nil
	}
	presented := hashSecret(secret)
	now := s.now().UTC()
	err = s.store.withSessionLocked(ctx, sid, func(tx pgx.Tx, sess session) error {
		if subtle.ConstantTimeCompare(presented, sess.RefreshHash) != 1 &&
			(sess.PrevRefreshHash == nil || subtle.ConstantTimeCompare(presented, sess.PrevRefreshHash) != 1) {
			return nil
		}
		return revokeSession(ctx, tx, sess.ID, "logout", now)
	})
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func (s *Service) Me(ctx context.Context, p Principal) (User, error) {
	u, err := s.store.userByID(ctx, p.UserID)
	if errors.Is(err, errNotFound) {
		return User{}, httpx.ErrUnauthorized
	}
	return u, err
}

// Authenticate verifies an access token. It does not hit the database: a
// revoked session stays usable until its access token expires (≤10 min).
func (s *Service) Authenticate(token string) (Principal, error) {
	p, err := s.signer.Verify(token, s.now())
	if err != nil {
		return Principal{}, httpx.ErrUnauthorized
	}
	return p, nil
}

func (s *Service) IssueTicket(ctx context.Context, p Principal) (string, error) {
	if err := s.limit(ctx, ruleTicket, p.SessionID.String()); err != nil {
		return "", err
	}
	return s.tickets.Issue(ctx, p)
}

func (s *Service) newSession(userID uuid.UUID, now time.Time) (session, string, error) {
	secret, err := newRefreshSecret()
	if err != nil {
		return session{}, "", err
	}
	sess := session{
		ID:          uuid.Must(uuid.NewV7()),
		UserID:      userID,
		RefreshHash: hashSecret(secret),
		RotatedAt:   now,
		ExpiresAt:   now.Add(refreshTTL),
	}
	return sess, encodeRefresh(sess.ID, secret), nil
}

func (s *Service) tokens(u User, sid uuid.UUID, refresh string, now time.Time) (Tokens, error) {
	access, err := s.signer.Issue(Principal{UserID: u.ID, SessionID: sid}, now)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: refresh, User: u}, nil
}

func parseIP(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a
}
