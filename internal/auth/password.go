package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. 64 MiB per hash: the hasher bounds concurrency so a
// burst of logins cannot exhaust memory.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

var errBadHash = errors.New("auth: malformed password hash")

type Hasher struct {
	sem chan struct{}
}

// NewHasher allows at most parallel hash computations at once.
func NewHasher(parallel int) *Hasher {
	return &Hasher{sem: make(chan struct{}, parallel)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns a PHC-formatted argon2id hash.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	h.release()
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches the encoded hash, using the
// parameters stored in the hash so they can be raised later.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, errBadHash
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want))) //nolint:gosec // len(want) is 32
	h.release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
