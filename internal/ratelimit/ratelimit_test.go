package ratelimit

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAllow(t *testing.T) {
	mr := miniredis.RunT(t)
	l := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	rule := Rule{Name: "login", Limit: 3, Window: time.Minute}

	for i := 0; i < 3; i++ {
		ok, _, err := l.Allow(t.Context(), rule, "1.2.3.4")
		if err != nil || !ok {
			t.Fatalf("event %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, retry, err := l.Allow(t.Context(), rule, "1.2.3.4")
	if err != nil || ok {
		t.Fatalf("4th event: ok=%v err=%v, want blocked", ok, err)
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry = %v, want within window", retry)
	}

	if ok, _, _ := l.Allow(t.Context(), rule, "5.6.7.8"); !ok {
		t.Fatal("other key must have its own budget")
	}

	mr.FastForward(time.Minute + time.Second)
	if ok, _, _ := l.Allow(t.Context(), rule, "1.2.3.4"); !ok {
		t.Fatal("window must reset after expiry")
	}
}

func TestKeyAlwaysHasTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	l := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	rule := Rule{Name: "x", Limit: 10, Window: time.Minute}
	_, _, _ = l.Allow(t.Context(), rule, "k")
	if ttl := mr.TTL("ratelimit:x:k"); ttl <= 0 {
		t.Fatalf("ttl = %v, want positive", ttl)
	}
}
