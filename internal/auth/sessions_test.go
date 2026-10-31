package auth

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testPassword = "s3cret-pass" // what register uses

// login signs name in on a new device with the given User-Agent.
func login(t *testing.T, e *env, name, ua string) *client {
	t.Helper()
	c := e.client(t)
	c.ua = ua
	resp, body := c.do("POST", "/api/v1/auth/login", map[string]string{"email": name + "@example.com", "password": testPassword})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	return c
}

func (e *env) sessionOf(t *testing.T, c *client) uuid.UUID {
	t.Helper()
	p, err := e.svc.Authenticate(t.Context(), c.access)
	if err != nil {
		t.Fatal(err)
	}
	return p.SessionID
}

// listSessions returns the caller's sessions as the API answers them.
func listSessions(t *testing.T, c *client) []map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", c.base+"/api/v1/users/@me/sessions", nil)
	req.AddCookie(&http.Cookie{Name: accessCookie, Value: c.access}) //nolint:gosec // request cookie in a test client
	var out []map[string]any
	decodeInto(t, req, &out)
	return out
}

// status is the answer to an authenticated GET /users/@me: 200 while the
// device is signed in, 401 once its session is revoked.
func status(c *client) int {
	resp, _ := c.do("GET", "/api/v1/users/@me", nil)
	return resp.StatusCode
}

func TestSessionsListAndRevoke(t *testing.T) {
	e := newEnv(t)
	a := e.client(t)
	a.ua = "Firefox on Linux"
	register(t, a, "timofey")
	b := login(t, e, "timofey", "Chrome on Windows")
	stranger := e.client(t)
	register(t, stranger, "stranger")

	list := listSessions(t, a)
	if len(list) != 2 {
		t.Fatalf("sessions = %v", list)
	}
	bySID := map[string]map[string]any{}
	for _, s := range list {
		bySID[s["id"].(string)] = s
	}
	mine, theirs := bySID[e.sessionOf(t, a).String()], bySID[e.sessionOf(t, b).String()]
	if mine == nil || theirs == nil {
		t.Fatalf("sessions %v lack a or b", list)
	}
	if mine["current"] != true || theirs["current"] != false {
		t.Fatalf("current flags: mine %v, theirs %v", mine["current"], theirs["current"])
	}
	if mine["device"] != "Firefox on Linux" || theirs["device"] != "Chrome on Windows" || theirs["ip"] != "127.0.0.1" {
		t.Fatalf("session fields: %v / %v", mine, theirs)
	}
	if n := e.countEvents(t, "SESSION_CREATE"); n != 1 {
		t.Fatalf("SESSION_CREATE events = %d, want 1 (login; registration has no other devices)", n)
	}

	bSID := e.sessionOf(t, b)
	for _, c := range []struct {
		name string
		by   *client
		id   string
		want int
	}{
		{"someone else's session", stranger, bSID.String(), http.StatusNotFound},
		{"unknown session", a, uuid.NewString(), http.StatusNotFound},
		{"malformed id", a, "nope", http.StatusNotFound},
		{"my other device", a, bSID.String(), http.StatusNoContent},
		{"already signed out", a, bSID.String(), http.StatusNotFound},
	} {
		if resp, body := c.by.do("DELETE", "/api/v1/users/@me/sessions/"+c.id, nil); resp.StatusCode != c.want {
			t.Fatalf("%s: %d %v, want %d", c.name, resp.StatusCode, body, c.want)
		}
	}
	// Signed out at once, not when its access token expires.
	if got := status(b); got != http.StatusUnauthorized {
		t.Fatalf("revoked device: %d, want 401 right away", got)
	}
	if resp, _ := b.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("revoked device refreshed")
	}
	if got := status(a); got != http.StatusOK {
		t.Fatalf("the device that signed b out: %d", got)
	}
	if len(listSessions(t, a)) != 1 {
		t.Fatal("revoked session still listed")
	}
}

func TestRevokeCurrentSessionSignsOut(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	register(t, c, "timofey")
	token := c.access
	if resp, _ := c.do("DELETE", "/api/v1/users/@me/sessions/"+e.sessionOf(t, c).String(), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke current: %d", resp.StatusCode)
	}
	if c.access != "" || c.refresh != "" {
		t.Fatal("cookies must be cleared like on logout")
	}
	again := &client{t: t, base: c.base, access: token}
	if got := status(again); got != http.StatusUnauthorized {
		t.Fatalf("old access token: %d", got)
	}
}

func TestRevokeOtherSessions(t *testing.T) {
	e := newEnv(t)
	me := e.client(t)
	register(t, me, "timofey")
	other1 := login(t, e, "timofey", "phone")
	other2 := login(t, e, "timofey", "laptop")

	if resp, _ := me.do("POST", "/api/v1/users/@me/sessions/revoke-others", map[string]string{"password": "wrong-pass"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	if status(other1) != http.StatusOK {
		t.Fatal("a refused request signed a device out")
	}
	resp, body := me.do("POST", "/api/v1/users/@me/sessions/revoke-others", map[string]string{"password": testPassword})
	if resp.StatusCode != http.StatusOK || body["revoked"] != float64(2) {
		t.Fatalf("revoke others: %d %v", resp.StatusCode, body)
	}
	for _, c := range []*client{other1, other2} {
		if got := status(c); got != http.StatusUnauthorized {
			t.Fatalf("other device: %d, want 401 right away", got)
		}
	}
	if status(me) != http.StatusOK {
		t.Fatal("the current device must stay signed in")
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	me := e.client(t)
	register(t, me, "timofey")
	other := login(t, e, "timofey", "phone")

	for _, c := range []struct {
		name, current, next string
		want                int
	}{
		{"wrong current password", "wrong-pass", "brand-new-pass", http.StatusForbidden},
		{"too short", testPassword, "short", http.StatusUnprocessableEntity},
		{"same as the email", testPassword, "timofey@example.com", http.StatusUnprocessableEntity},
		{"same as the username", testPassword, "TIMOFEY", http.StatusUnprocessableEntity},
	} {
		resp, body := me.do("POST", "/api/v1/users/@me/password", map[string]string{"current_password": c.current, "new_password": c.next})
		if resp.StatusCode != c.want {
			t.Fatalf("%s: %d %v, want %d", c.name, resp.StatusCode, body, c.want)
		}
		if status(other) != http.StatusOK {
			t.Fatalf("%s: a refused change signed a device out", c.name)
		}
	}

	resp, body := me.do("POST", "/api/v1/users/@me/password", map[string]string{"current_password": testPassword, "new_password": "brand-new-pass"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change: %d %v", resp.StatusCode, body)
	}
	if got := status(other); got != http.StatusUnauthorized {
		t.Fatalf("other device after a password change: %d, want 401", got)
	}
	if status(me) != http.StatusOK {
		t.Fatal("the current device must stay signed in")
	}
	fresh := e.client(t)
	if resp, _ := fresh.do("POST", "/api/v1/auth/login", map[string]string{"email": "timofey@example.com", "password": testPassword}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password: %d", resp.StatusCode)
	}
	if resp, _ := fresh.do("POST", "/api/v1/auth/login", map[string]string{"email": "timofey@example.com", "password": "brand-new-pass"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("new password: %d", resp.StatusCode)
	}
}

// Re-entering the password is limited per account, so a stolen session
// cannot be used to guess it.
func TestPasswordCheckLimit(t *testing.T) {
	e := newEnv(t)
	me := e.client(t)
	register(t, me, "timofey")
	for i := range rulePasswordCheck.Limit {
		if resp, _ := me.do("POST", "/api/v1/users/@me/sessions/revoke-others", map[string]string{"password": "wrong-pass"}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i+1, resp.StatusCode)
		}
	}
	// The limit is shared by every action that asks for the password.
	resp, _ := me.do("POST", "/api/v1/users/@me/password", map[string]string{"current_password": testPassword, "new_password": "brand-new-pass"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
}

// Signing in on one device more than maxSessions revokes the least
// recently used.
func TestSessionLimit(t *testing.T) {
	e := newEnv(t)
	me := e.client(t)
	register(t, me, "timofey")
	uid, err := uuid.Parse(me.userID(t))
	if err != nil {
		t.Fatal(err)
	}
	// The registration's session is used now; fill up with older ones.
	_, err = e.pg.Exec(t.Context(),
		`INSERT INTO sessions (id, user_id, refresh_hash, rotated_at, last_used_at, expires_at)
		 SELECT gen_random_uuid(), $1, '\x00', $2, $2 - make_interval(mins => g), $2 + interval '1 day'
		 FROM generate_series(1, $3) g`, uid, e.clock.Now(), maxSessions-1)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Second)
	login(t, e, "timofey", "one too many")

	var active int
	var oldestReason string
	if err := e.pg.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE revoked_at IS NULL),
		        (SELECT revoke_reason FROM sessions WHERE user_id = $1 ORDER BY last_used_at LIMIT 1)
		 FROM sessions WHERE user_id = $1`, uid).Scan(&active, &oldestReason); err != nil {
		t.Fatal(err)
	}
	if active != maxSessions || oldestReason != "session_limit" {
		t.Fatalf("active = %d, oldest revoked for %q; want %d and session_limit", active, oldestReason, maxSessions)
	}
	if status(me) != http.StatusOK {
		t.Fatal("a recently used session was revoked")
	}
}

// Without Valkey a revoked session cannot be told apart, so requests fail
// rather than let one through (like the rate limits).
func TestRevocationCheckFailsClosed(t *testing.T) {
	e := newEnv(t)
	me := e.client(t)
	register(t, me, "timofey")
	e.mr.SetError("down")
	got := status(me)
	e.mr.SetError("")
	if got != http.StatusInternalServerError {
		t.Fatalf("with Valkey down: %d, want 500", got)
	}
	if status(me) != http.StatusOK {
		t.Fatal("back up: the device must be signed in")
	}
}

func decodeInto(t *testing.T, req *http.Request, out any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func (e *env) countEvents(t *testing.T, typ string) int {
	t.Helper()
	var n int
	if err := e.pg.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE type = $1`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (c *client) userID(t *testing.T) string {
	t.Helper()
	_, body := c.do("GET", "/api/v1/users/@me", nil)
	id, _ := body["id"].(string)
	return id
}
