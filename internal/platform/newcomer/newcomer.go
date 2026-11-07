// Package newcomer holds back accounts made a few minutes ago from where
// strangers read them: guild channels, friend requests, new DMs
// (docs/adr/0044-newcomers.md). A raid of fresh accounts then has to wait,
// and a moderator has time to act; a real newcomer waits once.
package newcomer

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// Policy is how long a new account waits; zero turns it off.
type Policy struct {
	Wait time.Duration
	now  func() time.Time
}

// Left is how long the account still waits. Its age comes from its id, a
// UUIDv7 made at sign-up; an id of another version is an old account.
func (p Policy) Left(userID uuid.UUID) time.Duration {
	if p.Wait <= 0 || userID.Version() != 7 {
		return 0
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	sec, nsec := userID.Time().UnixTime()
	return max(time.Unix(sec, nsec).Add(p.Wait).Sub(now()), 0)
}

// Check answers 403 account_too_new with retry_after while the account
// waits; what names the action ("писать на серверах").
func (p Policy) Check(userID uuid.UUID, what string) error {
	left := p.Left(userID)
	if left <= 0 {
		return nil
	}
	e := httpx.Retry(http.StatusForbidden, "account_too_new", "", left)
	e.Message = fmt.Sprintf("Новые аккаунты могут %s через %s", what, minutes(e.RetryAfter))
	return e
}

// minutes says a wait in seconds the way people read it.
func minutes(secs int) string {
	if secs < 60 {
		return fmt.Sprintf("%d с", secs)
	}
	return fmt.Sprintf("%d мин", (secs+59)/60)
}
