package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/eastsidekillas/ruscord-core/internal/events"
)

// ephReceiver is what the hub delivers ephemeral events to; *session
// implements it.
type ephReceiver interface {
	ephemeral(ev event)
}

// hub holds one NATS subscription per ephemeral subject for all sessions of
// the node that need it, not one per session: a guild of 250 people on one
// node is one subscription, not 250.
type hub struct {
	nc  *nats.Conn
	log *slog.Logger

	mu   sync.Mutex
	subs map[string]*hubSub
}

type hubSub struct {
	sub       *nats.Subscription
	receivers map[ephReceiver]struct{}
}

func newHub(nc *nats.Conn, log *slog.Logger) *hub {
	return &hub{nc: nc, log: log, subs: map[string]*hubSub{}}
}

// sync makes rcv listen to exactly want, given it listened to have, and
// returns the new set. Failed subscriptions are left out of the result, so
// the next sync retries them.
func (h *hub) sync(rcv ephReceiver, have map[string]bool, want []string) (map[string]bool, error) {
	next := make(map[string]bool, len(want))
	var firstErr error
	for _, s := range want {
		if have[s] {
			next[s] = true
			continue
		}
		if err := h.add(rcv, s); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		next[s] = true
	}
	for s := range have {
		if !next[s] {
			h.remove(rcv, s)
		}
	}
	return next, firstErr
}

func (h *hub) add(rcv ephReceiver, subject string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.subs[subject]
	if hs == nil {
		hs = &hubSub{receivers: map[ephReceiver]struct{}{}}
		sub, err := h.nc.Subscribe(subject, func(m *nats.Msg) { h.deliver(subject, m) })
		if err != nil {
			return fmt.Errorf("gateway: subscribe %s: %w", subject, err)
		}
		hs.sub = sub
		h.subs[subject] = hs
	}
	hs.receivers[rcv] = struct{}{}
	return nil
}

func (h *hub) remove(rcv ephReceiver, subject string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.subs[subject]
	if hs == nil {
		return
	}
	delete(hs.receivers, rcv)
	if len(hs.receivers) == 0 {
		if err := hs.sub.Unsubscribe(); err != nil {
			h.log.Warn("gateway: unsubscribe", "subject", subject, "err", err)
		}
		delete(h.subs, subject)
	}
}

// subscribed reports how many subjects have a NATS subscription; for tests.
func (h *hub) subscribed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *hub) deliver(subject string, m *nats.Msg) {
	if !json.Valid(m.Data) {
		h.log.Warn("gateway: skipping ephemeral event with invalid JSON", "subject", subject)
		return
	}
	ev := event{Subject: subject, Type: m.Header.Get(events.HeaderType), Data: m.Data}
	h.mu.Lock()
	hs := h.subs[subject]
	var rcvs []ephReceiver
	if hs != nil {
		rcvs = make([]ephReceiver, 0, len(hs.receivers))
		for r := range hs.receivers {
			rcvs = append(rcvs, r)
		}
	}
	h.mu.Unlock()
	// Delivered outside the lock: a receiver may sync its own subjects.
	for _, r := range rcvs {
		r.ephemeral(ev)
	}
}
