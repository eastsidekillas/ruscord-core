package presence

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/events"
)

func TestVisible(t *testing.T) {
	cases := []struct {
		name     string
		choice   Status
		sessions []Status
		want     Status
	}{
		{"no sessions", Online, nil, Offline},
		{"no sessions, dnd", DND, nil, Offline},
		{"one active device", Online, []Status{Online}, Online},
		{"one active of two", Online, []Status{Idle, Online}, Online},
		{"every device idle", Online, []Status{Idle, Idle}, Idle},
		{"chose idle while active", Idle, []Status{Online}, Idle},
		{"dnd while active", DND, []Status{Online}, DND},
		{"dnd while idle", DND, []Status{Idle}, DND},
		{"invisible while active", Invisible, []Status{Online}, Offline},
		{"invisible while idle", Invisible, []Status{Idle}, Offline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Visible(c.choice, c.sessions); got != c.want {
				t.Fatalf("Visible(%s, %v) = %s, want %s", c.choice, c.sessions, got, c.want)
			}
		})
	}
}

func TestValid(t *testing.T) {
	for _, s := range []Status{Online, Idle, DND, Invisible} {
		if !ValidChoice(s) {
			t.Errorf("%s must be a valid choice", s)
		}
	}
	for _, s := range []Status{Offline, "", "away", "ONLINE"} {
		if ValidChoice(s) {
			t.Errorf("%q must not be a valid choice", s)
		}
	}
	if !ValidSession(Online) || !ValidSession(Idle) || ValidSession(DND) || ValidSession(Invisible) || ValidSession(Offline) {
		t.Error("a session reports only online or idle")
	}
}

type published struct {
	subject string
	upd     Update
}

type fakePub struct {
	mu   sync.Mutex
	msgs []published
	err  error
}

func (p *fakePub) Publish(subject, typ string, data any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if typ != events.PresenceUpdate {
		panic("unexpected type " + typ)
	}
	p.msgs = append(p.msgs, published{subject, data.(Update)})
	return nil
}

func (p *fakePub) take() []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.msgs
	p.msgs = nil
	return out
}

type fakeGuilds struct{ ids []uuid.UUID }

func (g fakeGuilds) SmallGuilds(_ context.Context, _ uuid.UUID, max int) ([]uuid.UUID, error) {
	if max != MaxGuildMembers {
		panic("unexpected max")
	}
	return g.ids, nil
}

func newTestService(t *testing.T, guilds ...uuid.UUID) (*Service, *fakePub, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	pub := &fakePub{}
	return NewService(redis.NewClient(&redis.Options{Addr: mr.Addr()}), pub, fakeGuilds{guilds}, time.Minute), pub, mr
}

func TestRefreshPublishesOnlyChanges(t *testing.T) {
	g1, g2 := uuid.New(), uuid.New()
	s, pub, _ := newTestService(t, g1, g2)
	ctx := t.Context()
	user := uuid.New()
	want := func(st Status) []published {
		return []published{
			{events.EphPresenceTopic(user), Update{user, st}},
			{events.EphGuildTopic(g1), Update{user, st}},
			{events.EphGuildTopic(g2), Update{user, st}},
		}
	}
	refresh := func(choice Status) Status {
		t.Helper()
		st, err := s.Refresh(ctx, user, choice)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Still offline: nothing to tell anyone.
	if st := refresh(Online); st != Offline || len(pub.take()) != 0 {
		t.Fatalf("offline user published %s", st)
	}
	if err := s.Touch(ctx, user, "a", Online); err != nil {
		t.Fatal(err)
	}
	if st := refresh(Online); st != Online || !slices.Equal(pub.take(), want(Online)) {
		t.Fatalf("first connection: %s", st)
	}
	// A second device does not change what others see.
	_ = s.Touch(ctx, user, "b", Idle)
	if refresh(Online); len(pub.take()) != 0 {
		t.Fatal("second device republished an unchanged status")
	}
	// The active device goes idle: now every device is idle.
	_ = s.Touch(ctx, user, "a", Idle)
	if st := refresh(Online); st != Idle || !slices.Equal(pub.take(), want(Idle)) {
		t.Fatalf("all idle: %s", st)
	}
	// Invisible looks like offline and never leaks a session.
	if st := refresh(Invisible); st != Offline || !slices.Equal(pub.take(), want(Offline)) {
		t.Fatalf("invisible: %s", st)
	}
	statuses, _ := s.Statuses(ctx, []uuid.UUID{user})
	if statuses[user] != Offline {
		t.Fatalf("invisible user reads as %s", statuses[user])
	}
	refresh(DND)
	pub.take()
	// Both devices leave.
	_ = s.Drop(ctx, user, "a")
	_ = s.Drop(ctx, user, "b")
	if st := refresh(DND); st != Offline || !slices.Equal(pub.take(), want(Offline)) {
		t.Fatalf("after leaving: %s", st)
	}
}

// A node that dies never drops its sessions; their keys expire and the next
// refresh prunes them, so nobody stays online forever.
func TestExpiredSessionsArePruned(t *testing.T) {
	s, pub, mr := newTestService(t)
	ctx := t.Context()
	user := uuid.New()
	_ = s.Touch(ctx, user, "dead", Online)
	_, _ = s.Refresh(ctx, user, Online)
	pub.take()
	mr.FastForward(2 * time.Minute)
	_ = s.Touch(ctx, user, "alive", Idle)
	if st, err := s.Refresh(ctx, user, Online); err != nil || st != Idle {
		t.Fatalf("with the dead session gone, want idle: %s %v", st, err)
	}
	if members := mr.Exists(indexKey(user)); !members {
		t.Fatal("index of a live user must exist")
	}
	if ids, _ := s.rdb.SMembers(ctx, indexKey(user)).Result(); !slices.Equal(ids, []string{"alive"}) {
		t.Fatalf("index = %v, want only the live session", ids)
	}
}

func TestStatuses(t *testing.T) {
	s, _, mr := newTestService(t)
	ctx := t.Context()
	on, off := uuid.New(), uuid.New()
	_ = s.Touch(ctx, on, "x", Online)
	_, _ = s.Refresh(ctx, on, Online)
	got, err := s.Statuses(ctx, []uuid.UUID{on, off})
	if err != nil || got[on] != Online || got[off] != Offline || len(got) != 2 {
		t.Fatalf("statuses = %v, %v", got, err)
	}
	if empty, err := s.Statuses(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatal("no users, no lookups")
	}
	mr.FastForward(2 * time.Minute) // no heartbeats: the last value expires too
	if got, _ := s.Statuses(ctx, []uuid.UUID{on}); got[on] != Offline {
		t.Fatalf("after the TTL: %s", got[on])
	}
}

func TestTouchRejectsChoices(t *testing.T) {
	s, _, _ := newTestService(t)
	for _, st := range []Status{DND, Invisible, Offline, "x"} {
		if err := s.Touch(t.Context(), uuid.New(), "s", st); err == nil {
			t.Errorf("Touch accepted %q", st)
		}
	}
}

func TestPublishErrorIsReported(t *testing.T) {
	s, pub, _ := newTestService(t)
	pub.err = errors.New("nats down")
	user := uuid.New()
	_ = s.Touch(t.Context(), user, "a", Online)
	if _, err := s.Refresh(t.Context(), user, Online); err == nil {
		t.Fatal("a failed publish must be reported")
	}
	// The change was not delivered, so the next refresh must try again
	// instead of believing it was published.
	pub.err = nil
	if _, err := s.Refresh(t.Context(), user, Online); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 1 || got[0].upd.Status != Online {
		t.Fatalf("retry after a failed publish sent %v", got)
	}
}
