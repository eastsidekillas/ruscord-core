package newcomer

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// idAt is a UUIDv7 made at t.
func idAt(t time.Time) uuid.UUID {
	u := uuid.Must(uuid.NewV7())
	ms := uint64(t.UnixMilli()) //nolint:gosec // test times are after 1970
	for i := range 6 {
		u[i] = byte(ms >> (40 - 8*i))
	}
	return u
}

func TestLeft(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	p := Policy{Wait: 5 * time.Minute, now: func() time.Time { return now }}
	for _, tt := range []struct {
		name string
		p    Policy
		id   uuid.UUID
		want time.Duration
	}{
		{"just signed up", p, idAt(now), 5 * time.Minute},
		{"two minutes ago", p, idAt(now.Add(-2 * time.Minute)), 3 * time.Minute},
		{"past the wait", p, idAt(now.Add(-6 * time.Minute)), 0},
		{"policy off", Policy{now: p.now}, idAt(now), 0},
		{"not a UUIDv7: an old account", p, uuid.New(), 0},
	} {
		if got := tt.p.Left(tt.id).Round(time.Second); got != tt.want {
			t.Errorf("%s: left %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	p := Policy{Wait: 5 * time.Minute, now: func() time.Time { return now }}
	for _, tt := range []struct {
		age        time.Duration
		retryAfter int
		message    string
	}{
		{0, 300, "Новые аккаунты могут писать на серверах через 5 мин"},
		{4*time.Minute + 30*time.Second, 30, "Новые аккаунты могут писать на серверах через 30 с"},
		{10 * time.Minute, 0, ""},
	} {
		err := p.Check(idAt(now.Add(-tt.age)), "писать на серверах")
		var e *httpx.Error
		if tt.retryAfter == 0 {
			if err != nil {
				t.Errorf("age %v: %v", tt.age, err)
			}
			continue
		}
		if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Code != "account_too_new" ||
			e.RetryAfter != tt.retryAfter || e.Message != tt.message {
			t.Errorf("age %v: %+v", tt.age, err)
		}
	}
}
