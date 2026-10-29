// Package apitest runs module HTTP handlers against a real Postgres for
// integration tests: a migrated schema, auth with test accounts, and a
// small JSON client.
package apitest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

type Env struct {
	T           *testing.T
	PG          *pgxpool.Pool
	Auth        *auth.Service
	Limiter     *ratelimit.Limiter
	RequireAuth func(http.Handler) http.Handler
	Mux         *http.ServeMux

	srv    *httptest.Server
	signer *auth.Signer
}

// New starts an isolated environment; the test runs in parallel with others.
// Modules register their handlers on Mux.
func New(t *testing.T) *Env {
	t.Helper()
	pg := testdb.New(t)
	t.Parallel()

	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	limiter := ratelimit.New(rdb)
	signer, _ := auth.NewSigner(make([]byte, 32))
	authSvc, err := auth.NewService(context.Background(), auth.Deps{
		PG: pg, Signer: signer, Limiter: limiter, Tickets: auth.NewTickets(rdb),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authHTTP := auth.NewHandler(authSvc, false)
	authHTTP.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Env{T: t, PG: pg, Auth: authSvc, Limiter: limiter, RequireAuth: authHTTP.RequireAuth,
		Mux: mux, srv: srv, signer: signer}
}

type User struct {
	Env   *Env
	ID    uuid.UUID
	token string
}

// User inserts an account directly (auth itself is tested in internal/auth)
// and signs an access token for it.
func (e *Env) User(name string) *User {
	e.T.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := e.PG.Exec(context.Background(),
		`INSERT INTO users (id, email, username, display_name, password_hash) VALUES ($1, $2, $3::text, $3::text, 'x')`,
		id, name+"@example.com", name); err != nil {
		e.T.Fatal(err)
	}
	tok, err := e.signer.Issue(auth.Principal{UserID: id, SessionID: uuid.New()}, time.Now())
	if err != nil {
		e.T.Fatal(err)
	}
	return &User{Env: e, ID: id, token: tok}
}

// Do sends a request and decodes the JSON answer into a generic value.
func (u *User) Do(method, path string, body any) (int, any) {
	t := u.Env.T
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, u.Env.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "rc_at", Value: u.token}) //nolint:gosec // request cookie in a test client
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Expect is Do that fails the test on an unexpected status.
func (u *User) Expect(want int, method, path string, body any) any {
	u.Env.T.Helper()
	got, out := u.Do(method, path, body)
	if got != want {
		u.Env.T.Fatalf("%s %s: status %d, want %d: %v", method, path, got, want, out)
	}
	return out
}

// OutboxTypes lists the event types written so far, oldest first.
func (e *Env) OutboxTypes() []string {
	e.T.Helper()
	rows, err := e.PG.Query(context.Background(), `SELECT type FROM outbox ORDER BY id`)
	if err != nil {
		e.T.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// Accessors for decoded JSON.
func Obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func List(v any) []any         { l, _ := v.([]any); return l }
func Str(v any) string         { s, _ := v.(string); return s }
