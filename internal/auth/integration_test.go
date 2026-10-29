package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	platformstore "github.com/eastsidekillas/ruscord-core/internal/platform/store"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Integration tests need a real Postgres: set RUSCORD_TEST_POSTGRES_URL.
// Each test gets its own schema, so tests can run in parallel.

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	srv   *httptest.Server
	svc   *Service
	pg    *pgxpool.Pool
	mr    *miniredis.Miniredis
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("RUSCORD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("RUSCORD_TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := platformstore.Migrate(ctx, pg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	signer, _ := NewSigner(make([]byte, 32))
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	svc, err := NewService(ctx, Deps{
		PG: pg, Signer: signer, Limiter: ratelimit.New(rdb), Tickets: NewTickets(rdb), Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(svc, false).Register(mux)
	srv := httptest.NewServer(httpx.CheckOrigin([]string{"http://localhost:4200"}, mux))
	t.Cleanup(srv.Close)
	return &env{srv: srv, svc: svc, pg: pg, mr: mr, clock: clk}
}

// client keeps cookies per path like a browser would.
type client struct {
	t       *testing.T
	base    string
	access  string
	refresh string
}

func (e *env) client(t *testing.T) *client { return &client{t: t, base: e.srv.URL} }

// result is the part of a response the tests look at; the body is already
// read and closed by do.
type result struct{ StatusCode int }

func (c *client) do(method, path string, body any) (result, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(c.t.Context(), method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.access != "" {
		req.AddCookie(&http.Cookie{Name: accessCookie, Value: c.access}) //nolint:gosec // request cookie in a test client
	}
	if c.refresh != "" && strings.HasPrefix(path, refreshCookiePath) {
		req.AddCookie(&http.Cookie{Name: refreshCookie, Value: c.refresh}) //nolint:gosec // request cookie in a test client
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case accessCookie:
			c.access = ck.Value
			if ck.MaxAge < 0 {
				c.access = ""
			}
		case refreshCookie:
			if ck.Path != refreshCookiePath {
				c.t.Errorf("refresh cookie path = %q", ck.Path)
			}
			if !ck.HttpOnly {
				c.t.Error("refresh cookie must be HttpOnly")
			}
			c.refresh = ck.Value
			if ck.MaxAge < 0 {
				c.refresh = ""
			}
		}
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{StatusCode: resp.StatusCode}, out
}

func register(t *testing.T, c *client, name string) map[string]any {
	t.Helper()
	resp, body := c.do("POST", "/api/v1/auth/register", map[string]string{
		"email": name + "@example.com", "username": name, "password": "s3cret-pass",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", resp.StatusCode, body)
	}
	return body
}

func TestRegisterLoginMe(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	u := register(t, c, "timofey")
	if _, leaked := u["password_hash"]; leaked {
		t.Fatal("password hash in response")
	}
	if c.access == "" || c.refresh == "" {
		t.Fatal("register must sign the user in")
	}

	resp, me := c.do("GET", "/api/v1/users/@me", nil)
	if resp.StatusCode != 200 || me["username"] != "timofey" {
		t.Fatalf("@me: %d %v", resp.StatusCode, me)
	}

	c2 := e.client(t)
	resp, _ = c2.do("POST", "/api/v1/auth/login", map[string]string{"email": "TIMOFEY@example.com", "password": "s3cret-pass"})
	if resp.StatusCode != 200 {
		t.Fatalf("login (case-insensitive email): %d", resp.StatusCode)
	}
}

func TestRegisterDuplicates(t *testing.T) {
	e := newEnv(t)
	register(t, e.client(t), "timofey")
	for _, tc := range []struct{ email, username, code string }{
		{"Timofey@example.com", "other", "email_taken"},
		{"other@example.com", "TIMOFEY", "username_taken"},
	} {
		resp, body := e.client(t).do("POST", "/api/v1/auth/register", map[string]string{
			"email": tc.email, "username": tc.username, "password": "s3cret-pass",
		})
		if resp.StatusCode != http.StatusConflict || body["code"] != tc.code {
			t.Errorf("%s/%s: %d %v, want 409 %s", tc.email, tc.username, resp.StatusCode, body, tc.code)
		}
	}
}

func TestLoginWrongPasswordAndUnknownEmailLookAlike(t *testing.T) {
	e := newEnv(t)
	register(t, e.client(t), "timofey")
	r1, b1 := e.client(t).do("POST", "/api/v1/auth/login", map[string]string{"email": "timofey@example.com", "password": "wrong-pass"})
	r2, b2 := e.client(t).do("POST", "/api/v1/auth/login", map[string]string{"email": "ghost@example.com", "password": "wrong-pass"})
	if r1.StatusCode != 401 || r2.StatusCode != 401 || b1["code"] != b2["code"] || b1["message"] != b2["message"] {
		t.Fatalf("responses differ: %d %v / %d %v", r1.StatusCode, b1, r2.StatusCode, b2)
	}
}

func TestLoginRateLimitPerEmail(t *testing.T) {
	e := newEnv(t)
	register(t, e.client(t), "timofey")
	var last result
	var body map[string]any
	for i := 0; i < int(ruleLoginEmail.Limit)+1; i++ {
		last, body = e.client(t).do("POST", "/api/v1/auth/login", map[string]string{"email": "timofey@example.com", "password": "wrong-pass"})
	}
	if last.StatusCode != http.StatusTooManyRequests || body["code"] != "rate_limited" {
		t.Fatalf("got %d %v, want 429", last.StatusCode, body)
	}
}

func TestRefreshRotation(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	register(t, c, "timofey")
	first := c.refresh

	e.clock.Add(time.Minute)
	if resp, _ := c.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 200 {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	if c.refresh == first {
		t.Fatal("refresh token was not rotated")
	}
	if resp, _ := c.do("GET", "/api/v1/users/@me", nil); resp.StatusCode != 200 {
		t.Fatal("new access token rejected")
	}
}

func TestRefreshConcurrentTabsWithinGrace(t *testing.T) {
	e := newEnv(t)
	tabA := e.client(t)
	register(t, tabA, "timofey")
	tabB := &client{t: t, base: tabA.base, refresh: tabA.refresh}

	if resp, _ := tabA.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 200 {
		t.Fatal("tab A refresh failed")
	}
	e.clock.Add(5 * time.Second)
	if resp, body := tabB.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 200 {
		t.Fatalf("tab B refresh within grace: %d %v", resp.StatusCode, body)
	}
}

func TestRefreshReuseRevokesSession(t *testing.T) {
	e := newEnv(t)
	victim := e.client(t)
	register(t, victim, "timofey")
	stolen := victim.refresh

	if resp, _ := victim.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 200 {
		t.Fatal("victim refresh failed")
	}
	e.clock.Add(refreshGrace + time.Second)

	attacker := &client{t: t, base: victim.base, refresh: stolen}
	resp, body := attacker.do("POST", "/api/v1/auth/refresh", nil)
	if resp.StatusCode != 401 || body["code"] != "session_expired" {
		t.Fatalf("stale token: %d %v, want 401", resp.StatusCode, body)
	}
	if resp, _ := victim.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 401 {
		t.Fatal("session must be revoked for the victim too after reuse")
	}
	var reason string
	_ = e.pg.QueryRow(t.Context(), "SELECT revoke_reason FROM sessions").Scan(&reason)
	if reason != "refresh_reuse" {
		t.Fatalf("revoke_reason = %q", reason)
	}
}

func TestRefreshExpired(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	register(t, c, "timofey")
	e.clock.Add(refreshTTL + time.Hour)
	if resp, _ := c.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 401 {
		t.Fatalf("expired refresh: %d", resp.StatusCode)
	}
	if c.refresh != "" {
		t.Fatal("cookies must be cleared on expired session")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	register(t, c, "timofey")
	token := c.refresh
	if resp, _ := c.do("POST", "/api/v1/auth/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if c.access != "" || c.refresh != "" {
		t.Fatal("logout must clear cookies")
	}
	again := &client{t: t, base: c.base, refresh: token}
	if resp, _ := again.do("POST", "/api/v1/auth/refresh", nil); resp.StatusCode != 401 {
		t.Fatal("refresh after logout must fail")
	}
	// Logging out with garbage is still a success.
	junk := &client{t: t, base: c.base, refresh: "garbage"}
	if resp, _ := junk.do("POST", "/api/v1/auth/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("logout with bad token must succeed")
	}
}

func TestMeRequiresAuth(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	if resp, _ := c.do("GET", "/api/v1/users/@me", nil); resp.StatusCode != 401 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	c.access = "not-a-jwt"
	if resp, _ := c.do("GET", "/api/v1/users/@me", nil); resp.StatusCode != 401 {
		t.Fatalf("bad cookie: %d", resp.StatusCode)
	}
}

func TestGatewayTicketSingleUse(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	register(t, c, "timofey")
	resp, body := c.do("POST", "/api/v1/gateway/ticket", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("ticket: %d %v", resp.StatusCode, body)
	}
	ticket, _ := body["ticket"].(string)

	p, err := e.svc.tickets.Consume(t.Context(), ticket)
	if err != nil || p.UserID == uuid.Nil {
		t.Fatalf("consume: %v %v", p, err)
	}
	if _, err := e.svc.tickets.Consume(t.Context(), ticket); err != ErrInvalidTicket { //nolint:errorlint // sentinel
		t.Fatalf("second consume: %v, want ErrInvalidTicket", err)
	}

	resp, body = c.do("POST", "/api/v1/gateway/ticket", nil)
	ticket, _ = body["ticket"].(string)
	e.mr.FastForward(ticketTTL + time.Second)
	if _, err := e.svc.tickets.Consume(t.Context(), ticket); err != ErrInvalidTicket { //nolint:errorlint // sentinel
		t.Fatalf("expired ticket: %v (status %d)", err, resp.StatusCode)
	}
}

func TestForeignOriginRejected(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]string{"email": "a@b.io", "password": "x"})
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/api/v1/auth/login", strings.NewReader(string(b)))
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d, want 403", resp.StatusCode)
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	e := newEnv(t)
	resp, body := e.client(t).do("POST", "/api/v1/auth/register", map[string]any{
		"email": "a@b.io", "username": "abc", "password": "s3cret-pass", "is_admin": true,
	})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown field: %d %v", resp.StatusCode, body)
	}
}
