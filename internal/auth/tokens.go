package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	accessTTL  = 10 * time.Minute
	refreshTTL = 30 * 24 * time.Hour
	issuer     = "ruscord"
)

// Principal is the authenticated caller.
type Principal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
}

type accessClaims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// Signer issues and verifies access tokens (JWT, EdDSA). Any module that has
// the public key can verify a token without a database round trip.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("auth: ed25519 seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

func (s *Signer) Issue(p Principal, now time.Time) (string, error) {
	claims := accessClaims{
		SessionID: p.SessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   p.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(s.priv)
}

func (s *Signer) Verify(token string, now time.Time) (Principal, error) {
	var claims accessClaims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return s.pub, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return Principal{}, err
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Principal{}, err
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return Principal{}, err
	}
	return Principal{UserID: uid, SessionID: sid}, nil
}

// Refresh tokens are opaque: base64url(session id || 32 random bytes).
// Only sha256(secret) is stored, so a database leak does not leak sessions.
const refreshSecretLen = 32

var errBadRefresh = errors.New("auth: malformed refresh token")

func newRefreshSecret() ([]byte, error) {
	b := make([]byte, refreshSecretLen)
	_, err := rand.Read(b)
	return b, err
}

func encodeRefresh(sessionID uuid.UUID, secret []byte) string {
	buf := make([]byte, 0, 16+len(secret))
	buf = append(buf, sessionID[:]...)
	buf = append(buf, secret...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func decodeRefresh(token string) (uuid.UUID, []byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 16+refreshSecretLen {
		return uuid.Nil, nil, errBadRefresh
	}
	id, err := uuid.FromBytes(raw[:16])
	if err != nil {
		return uuid.Nil, nil, errBadRefresh
	}
	return id, raw[16:], nil
}

func hashSecret(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}
