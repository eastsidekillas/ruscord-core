package voice

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// callPrefix names the LiveKit room of a DM call (ADR-0007).
const callPrefix = "call:"

func callRoom(callID uuid.UUID) string { return callPrefix + callID.String() }

func callOf(room string) (uuid.UUID, bool) {
	id, ok := strings.CutPrefix(room, callPrefix)
	if !ok {
		return uuid.Nil, false
	}
	u, err := uuid.Parse(id)
	return u, err == nil
}

// Calls is what voice needs from the social module for DM calls
// (docs/adr/0032-dm-calls.md); social.Service implements it.
type Calls interface {
	CallAccess(ctx context.Context, userID, channelID uuid.UUID) (perm.Permissions, error)
	StartCall(ctx context.Context, userID, channelID uuid.UUID) (social.Call, error)
	DeclineCall(ctx context.Context, userID, channelID uuid.UUID) error
	CallJoined(ctx context.Context, userID, callID uuid.UUID, participant string) (bool, error)
	CallLeft(ctx context.Context, userID, callID uuid.UUID, participant string) (bool, error)
	CallMembers(ctx context.Context) ([]social.CallMember, error)
	CallsOf(ctx context.Context, userID uuid.UUID) ([]social.CallMember, error)
	EndIdleCalls(ctx context.Context) ([]uuid.UUID, error)
}

// WithCalls connects the social module; without it there are no DM calls.
func (s *Service) WithCalls(c Calls) { s.calls = c }

var errNoCalls = errors.New("voice: calls are not connected")

// JoinCall starts the call of a DM channel or joins the one going on, and
// issues a token for its room. Being in the call starts when LiveKit
// reports the connection (Webhook).
func (s *Service) JoinCall(ctx context.Context, userID, channelID uuid.UUID) (JoinInfo, error) {
	if s.calls == nil {
		return JoinInfo{}, errNoCalls
	}
	if err := s.limiter.Check(ctx, ruleJoin, userID.String()); err != nil {
		return JoinInfo{}, err
	}
	p, err := s.calls.CallAccess(ctx, userID, channelID)
	if err != nil {
		return JoinInfo{}, err
	}
	c, err := s.calls.StartCall(ctx, userID, channelID)
	if err != nil {
		return JoinInfo{}, err
	}
	info, err := s.token(ctx, userID, callRoom(c.ID), p, allowedSources(p, false), false)
	if err != nil {
		return JoinInfo{}, err
	}
	info.Call = &c
	return info, nil
}

// DeclineCall stops the ringing of a call for the user.
func (s *Service) DeclineCall(ctx context.Context, userID, channelID uuid.UUID) error {
	if s.calls == nil {
		return errNoCalls
	}
	return s.calls.DeclineCall(ctx, userID, channelID)
}

// callEvent applies a webhook about a call's room.
func (s *Service) callEvent(ctx context.Context, event string, userID, callID uuid.UUID, sid string) error {
	if s.calls == nil {
		return nil
	}
	switch event {
	case "participant_joined":
		ok, err := s.calls.CallJoined(ctx, userID, callID, sid)
		if err != nil || ok {
			return err
		}
		s.log.InfoContext(ctx, "voice: removing a participant who may not be in the call", "user", userID, "call", callID)
		return s.rooms.RemoveParticipant(ctx, callRoom(callID), userID.String())
	case "participant_left":
		return s.callLeft(ctx, userID, callID, sid)
	}
	return nil
}

// callLeft records a leave; the room of an ended call is closed.
func (s *Service) callLeft(ctx context.Context, userID, callID uuid.UUID, sid string) error {
	ended, err := s.calls.CallLeft(ctx, userID, callID, sid)
	if err != nil || !ended {
		return err
	}
	return s.rooms.DeleteRoom(ctx, callRoom(callID))
}

// reconcileCalls brings calls in line with LiveKit: members who are not in
// the room any more leave, those who may not be there (left the group,
// blocked) are removed, calls nobody joined while ringing end.
func (s *Service) reconcileCalls(ctx context.Context) error {
	if s.calls == nil {
		return nil
	}
	members, err := s.calls.CallMembers(ctx)
	if err != nil {
		return err
	}
	live := map[uuid.UUID]map[string]bool{}
	var errs []error
	for _, m := range members {
		here, ok := live[m.CallID]
		if !ok {
			ps, err := s.rooms.ListParticipants(ctx, callRoom(m.CallID))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			here = map[string]bool{}
			for _, p := range ps {
				here[p.Sid] = true
			}
			live[m.CallID] = here
		}
		if !m.Allowed && here[m.Participant] {
			s.log.InfoContext(ctx, "voice: removing someone who may not be in the call", "user", m.UserID, "call", m.CallID)
			if err := s.rooms.RemoveParticipant(ctx, callRoom(m.CallID), m.UserID.String()); err != nil {
				errs = append(errs, err)
				continue
			}
		} else if here[m.Participant] {
			continue
		}
		if err := s.callLeft(ctx, m.UserID, m.CallID, m.Participant); err != nil {
			errs = append(errs, err)
		}
	}
	ended, err := s.calls.EndIdleCalls(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, id := range ended {
		if err := s.rooms.DeleteRoom(ctx, callRoom(id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
