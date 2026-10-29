package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const ticketTTL = 30 * time.Second

var ErrInvalidTicket = errors.New("auth: invalid or used gateway ticket")

// Tickets are single-use credentials for opening a gateway WebSocket. They
// keep long-lived tokens out of URLs and out of JavaScript.
type Tickets struct {
	rdb *redis.Client
}

func NewTickets(rdb *redis.Client) *Tickets { return &Tickets{rdb: rdb} }

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return "gwticket:" + hex.EncodeToString(sum[:])
}

func (t *Tickets) Issue(ctx context.Context, p Principal) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	val, err := json.Marshal(map[string]string{"uid": p.UserID.String(), "sid": p.SessionID.String()})
	if err != nil {
		return "", err
	}
	if err := t.rdb.Set(ctx, ticketKey(ticket), val, ticketTTL).Err(); err != nil {
		return "", fmt.Errorf("ticket: %w", err)
	}
	return ticket, nil
}

// Consume returns the principal for a ticket and deletes it atomically, so
// a ticket can open at most one connection.
func (t *Tickets) Consume(ctx context.Context, ticket string) (Principal, error) {
	val, err := t.rdb.GetDel(ctx, ticketKey(ticket)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Principal{}, ErrInvalidTicket
	}
	if err != nil {
		return Principal{}, fmt.Errorf("ticket: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(val, &m); err != nil {
		return Principal{}, ErrInvalidTicket
	}
	var p Principal
	if p.UserID, err = uuid.Parse(m["uid"]); err != nil {
		return Principal{}, ErrInvalidTicket
	}
	if p.SessionID, err = uuid.Parse(m["sid"]); err != nil {
		return Principal{}, ErrInvalidTicket
	}
	return p, nil
}
