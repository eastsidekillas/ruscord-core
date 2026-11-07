package message

import (
	"net/http"
	"testing"
	"time"

	"github.com/eastsidekillas/ruscord-core/internal/platform/newcomer"
)

// Slow mode (ADR-0043): a member waits between messages in a slow channel;
// moderators and DMs do not.
func TestSlowMode(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	_, text, _ := e.server(owner, bob)
	path := "/api/v1/channels/" + text

	// Only those who manage the channel set it, and only on text channels.
	bob.Expect(http.StatusForbidden, "PATCH", path, map[string]any{"rate_limit_per_user": 30})
	for _, bad := range []int{-1, 21601} {
		owner.Expect(http.StatusUnprocessableEntity, "PATCH", path, map[string]any{"rate_limit_per_user": bad})
	}
	ch := obj(owner.Expect(http.StatusOK, "PATCH", path, map[string]any{"rate_limit_per_user": 30}))
	if ch["rate_limit_per_user"] != float64(30) {
		t.Fatalf("channel: %v", ch)
	}

	send(bob, text, "первое")
	r := obj(bob.Expect(http.StatusTooManyRequests, "POST", msgs(text), map[string]any{"content": "второе"}))
	if r["code"] != "slow_mode" || r["retry_after"].(float64) < 29 || r["retry_after"].(float64) > 30 {
		t.Fatalf("second message: %v", r)
	}
	// Moderators keep order and are not limited.
	for range 3 {
		send(owner, text, "модератор пишет")
	}
	// DMs have no slow mode.
	dm := e.dm(owner, bob)
	for range 2 {
		send(bob, dm, "в личке")
	}
	// Off again: Bob writes at once.
	owner.Expect(http.StatusOK, "PATCH", path, map[string]any{"rate_limit_per_user": 0})
	send(bob, text, "снова свободно")
}

// New accounts wait before writing in guild channels (ADR-0044); the owner
// and DMs with friends do not.
func TestNewcomersWait(t *testing.T) {
	e := newEnv(t)
	e.messages.SetNewcomers(newcomer.Policy{Wait: time.Hour})
	owner, bob := e.User("owner"), e.User("bob")
	_, text, _ := e.server(owner, bob)

	r := obj(bob.Expect(http.StatusForbidden, "POST", msgs(text), map[string]any{"content": "привет всем"}))
	if r["code"] != "account_too_new" || r["retry_after"].(float64) < 3500 {
		t.Fatalf("newcomer: %v", r)
	}
	if e.count(text) != 0 {
		t.Fatal("the message was written")
	}
	send(owner, text, "владелец сервера пишет сразу")
	send(bob, e.dm(owner, bob), "другу в личку можно")
}
