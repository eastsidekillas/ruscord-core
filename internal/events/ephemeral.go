package events

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Ephemeral events (typing, presence) skip the outbox and the EVENTS
// stream: they go over plain NATS subjects, at most once, and are never
// replayed. Losing one is harmless; writing every keystroke to Postgres is
// not (docs/adr/0018-ephemeral-events.md).
const (
	TypingStart    = "TYPING_START"
	PresenceUpdate = "PRESENCE_UPDATE"
)

// Ephemeral subjects. The "eph." prefix keeps them outside the stream's
// subjects (guild.>, user.>, channel.>).
func EphGuildTopic(id uuid.UUID) string    { return "eph.guild." + id.String() }
func EphChannelTopic(id uuid.UUID) string  { return "eph.channel." + id.String() }
func EphPresenceTopic(id uuid.UUID) string { return "eph.presence." + id.String() }

// EphemeralPublisher is what modules need to send ephemeral events;
// *Ephemeral implements it, tests use fakes.
type EphemeralPublisher interface {
	Publish(subject, typ string, data any) error
}

// Ephemeral publishes events that may be lost.
type Ephemeral struct {
	nc *nats.Conn
}

func NewEphemeral(nc *nats.Conn) *Ephemeral { return &Ephemeral{nc: nc} }

// Publish sends one event to subject with the same header as stream events.
func (e *Ephemeral) Publish(subject, typ string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("events: marshal %s: %w", typ, err)
	}
	msg := &nats.Msg{Subject: subject, Data: payload, Header: nats.Header{}}
	msg.Header.Set(HeaderType, typ)
	if err := e.nc.PublishMsg(msg); err != nil {
		return fmt.Errorf("events: publish %s: %w", typ, err)
	}
	return nil
}
