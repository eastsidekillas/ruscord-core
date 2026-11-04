package stats_test

import (
	"bytes"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/message"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
	"github.com/eastsidekillas/ruscord-core/internal/social"
	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

func TestRef(t *testing.T) {
	for in, want := range map[string]string{
		"stream": "stream", " Stream ": "stream", "tg_post-2": "tg_post-2",
		"": "", "стрим": "", "a b": "", "x?y": "", "0123456789012345678901234567890123": "",
	} {
		if got := stats.Ref(in); got != want {
			t.Errorf("Ref(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstID(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	first := stats.FirstID(at)
	if first.Version() != 7 || first.Variant() != uuid.RFC4122 {
		t.Fatalf("not a UUIDv7: %s", first)
	}
	if sec, _ := first.Time().UnixTime(); sec != at.Unix() {
		t.Fatalf("time %d, want %d", sec, at.Unix())
	}
	// Every id made at that moment or later sorts after it, every earlier one before.
	later := uuid.Must(uuid.NewV7())
	if later.String() < first.String() || stats.FirstID(time.Now().Add(time.Hour)).String() < later.String() {
		t.Fatalf("order: %s %s", first, later)
	}
}

func TestDay(t *testing.T) {
	// 22:30 UTC is already the next day in Moscow (UTC+3).
	if d := stats.Day(time.Date(2026, 11, 4, 22, 30, 0, 0, time.UTC)); d != "2026-11-05" {
		t.Fatalf("day = %s", d)
	}
}

func TestReport(t *testing.T) {
	e := apitest.New(t)
	ctx := t.Context()
	owner, bob := e.User("owner"), e.User("bob")
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	socials := social.NewService(e.PG, e.Auth, guilds, e.Limiter)
	messages := message.NewService(e.PG, e.Auth, guilds, socials, e.Limiter, e.Eph)
	pres := presence.NewService(e.Redis, e.Eph, guilds, time.Minute)
	svc := stats.NewService(e.PG, e.Limiter, []uuid.UUID{owner.ID}, e.Auth, guilds, socials, messages, pres)
	stats.NewHandler(svc, e.RequireAuth, auth.UserID).Register(e.Mux)

	// Two visits through the stream link from one address count once.
	for range 2 {
		visit(t, e, `{"ref":"Stream"}`)
	}
	// Sign-ups: one through the link, one with a tag that is not one.
	for name, ref := range map[string]string{"viewer": "stream", "other": "<script>"} {
		if _, err := e.Auth.Register(ctx, auth.RegisterInput{
			Email: name + "@example.com", Username: name, Password: apitest.Password + "x", Ref: ref,
		}, auth.ClientInfo{IP: "10.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	// Bob opens the app, is online and writes.
	bob.Ticket()
	if err := pres.Touch(ctx, bob.ID, "s1", presence.Online); err != nil {
		t.Fatal(err)
	}
	g, err := guilds.Create(ctx, bob.ID, guild.CreateGuildInput{Name: "сервер"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := messages.Send(ctx, bob.ID, g.Channels[0].ID, message.SendInput{Content: "привет"}); err != nil {
		t.Fatal(err)
	}

	bob.Expect(http.StatusNotFound, "GET", "/api/v1/admin/stats", nil)
	owner.Expect(http.StatusUnprocessableEntity, "GET", "/api/v1/admin/stats?days=3", nil)
	r := apitest.Obj(owner.Expect(http.StatusOK, "GET", "/api/v1/admin/stats?days=7", nil))

	days := apitest.List(r["days"])
	if len(days) != 7 || days[6] != stats.Day(time.Now()) || r["zone"] != stats.Zone {
		t.Fatalf("days %v, zone %v", days, r["zone"])
	}
	want := map[string]map[string]float64{
		"now":    {"online": 1, "sessions": 1, "active_day": 1, "active_week": 1, "in_voice": 0, "in_calls": 0},
		"totals": {"users": 4, "guilds": 1, "messages": 1},
	}
	for section, ns := range want {
		got := apitest.Obj(r[section])
		for k, n := range ns {
			if got[k] != n {
				t.Errorf("%s.%s = %v, want %v", section, k, got[k], n)
			}
		}
	}
	series := apitest.Obj(r["series"])
	for name, n := range map[string]float64{"registrations": 4, "active_users": 1, "messages": 1, "senders": 1, "guilds": 1, "joins": 1, "visits": 1} {
		s := apitest.List(series[name])
		if len(s) != 7 || s[6] != n || slices.ContainsFunc(s[:6], func(v any) bool { return v != float64(0) }) {
			t.Errorf("series %s = %v, want %v today", name, s, n)
		}
	}
	refs := map[string]map[string]any{}
	for _, row := range apitest.List(r["refs"]) {
		o := apitest.Obj(row)
		refs[apitest.Str(o["ref"])] = apitest.Obj(o["counts"])
	}
	if c := refs["stream"]; c["visits"] != float64(1) || c["registrations"] != float64(1) {
		t.Errorf("stream = %v", c)
	}
	if c := refs[""]; c["registrations"] != float64(3) {
		t.Errorf("no tag = %v", c)
	}
}

// visit posts a visit as an anonymous browser.
func visit(t *testing.T, e *apitest.Env, body string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.URL()+"/api/v1/visits", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("visit: %d", resp.StatusCode)
	}
}
