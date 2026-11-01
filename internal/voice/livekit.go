package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// LiveKit is the part of the LiveKit server API the core uses: access tokens,
// RoomService over HTTP and webhook verification. The formats follow
// livekit/protocol (auth/accesstoken.go, webhook/verifier.go): HS256 JWTs
// signed with the API secret, RoomService as Twirp with JSON bodies.
type LiveKit struct {
	api    string // http(s) base URL of the server
	key    string
	secret string
	http   *http.Client
	now    func() time.Time
}

// NewLiveKit talks to the server at url (ws:// or http://, as configured).
func NewLiveKit(url, key, secret string) *LiveKit {
	api := strings.TrimRight(url, "/")
	api = strings.Replace(strings.Replace(api, "wss://", "https://", 1), "ws://", "http://", 1)
	return &LiveKit{api: api, key: key, secret: secret, http: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

// videoGrant is the "video" claim; field names are LiveKit's.
type videoGrant struct {
	RoomJoin             bool     `json:"roomJoin,omitempty"`
	RoomAdmin            bool     `json:"roomAdmin,omitempty"`
	RoomList             bool     `json:"roomList,omitempty"`
	Room                 string   `json:"room,omitempty"`
	CanPublish           *bool    `json:"canPublish,omitempty"`
	CanPublishData       *bool    `json:"canPublishData,omitempty"`
	CanPublishSources    []string `json:"canPublishSources,omitempty"`
	CanSubscribe         *bool    `json:"canSubscribe,omitempty"`
	CanUpdateOwnMetadata *bool    `json:"canUpdateOwnMetadata,omitempty"`
}

type claims struct {
	jwt.RegisteredClaims
	Name     string      `json:"name,omitempty"`
	Metadata string      `json:"metadata,omitempty"`
	Video    *videoGrant `json:"video,omitempty"`
	Sha256   string      `json:"sha256,omitempty"`
}

// Join describes a participant entering a room.
type Join struct {
	Room     string
	Identity string
	Name     string
	Metadata string
	// Sources the participant may publish (microphone, camera,
	// screen_share, screen_share_audio); none means listen only.
	Sources []string
	TTL     time.Duration
}

// JoinToken is an access token for one room. Clients never get roomAdmin
// and cannot change their own name or metadata (docs/adr/0025-voice.md).
func (l *LiveKit) JoinToken(j Join) (string, error) {
	yes, no := true, false
	g := &videoGrant{RoomJoin: true, Room: j.Room, CanSubscribe: &yes, CanPublishData: &no, CanUpdateOwnMetadata: &no}
	publish := len(j.Sources) > 0
	g.CanPublish = &publish
	if publish {
		g.CanPublishSources = j.Sources
	}
	return l.sign(claims{Name: j.Name, Metadata: j.Metadata, Video: g}, j.Identity, j.TTL)
}

func (l *LiveKit) sign(c claims, subject string, ttl time.Duration) (string, error) {
	if l.key == "" || l.secret == "" {
		return "", errors.New("livekit: API key and secret are not configured")
	}
	now := l.now()
	c.RegisteredClaims = jwt.RegisteredClaims{
		Issuer:    l.key,
		Subject:   subject,
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(l.secret))
}

// Participant is what RoomService reports about someone in a room.
type Participant struct {
	Sid      string `json:"sid"`
	Identity string `json:"identity"`
}

// ErrNotFound: no such room or participant.
var ErrNotFound = errors.New("livekit: not found")

// ListParticipants returns who is in a room now; a room that does not exist
// is empty.
func (l *LiveKit) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	var out struct {
		Participants []Participant `json:"participants"`
	}
	err := l.call(ctx, "ListParticipants", map[string]string{"room": room}, &out, admin(room))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out.Participants, err
}

// ListRooms returns the names of the rooms LiveKit has now.
func (l *LiveKit) ListRooms(ctx context.Context) ([]string, error) {
	var out struct {
		Rooms []struct {
			Name string `json:"name"`
		} `json:"rooms"`
	}
	if err := l.call(ctx, "ListRooms", map[string]any{}, &out, &videoGrant{RoomList: true}); err != nil {
		return nil, err
	}
	names := make([]string, len(out.Rooms))
	for i, r := range out.Rooms {
		names[i] = r.Name
	}
	return names, nil
}

// RemoveParticipant disconnects identity from room; someone who is not
// there is not an error.
func (l *LiveKit) RemoveParticipant(ctx context.Context, room, identity string) error {
	err := l.call(ctx, "RemoveParticipant", map[string]string{"room": room, "identity": identity}, nil, admin(room))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func admin(room string) *videoGrant { return &videoGrant{RoomAdmin: true, Room: room} }

// call invokes a RoomService method with a short-lived token of grant.
func (l *LiveKit) call(ctx context.Context, method string, in, out any, grant *videoGrant) error {
	token, err := l.sign(claims{Video: grant}, "ruscord-core", time.Minute)
	if err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	// The URL is RUSCORD_LIVEKIT_URL and a method name from this file.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.api+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body)) //nolint:gosec // G704: configured LiveKit address, not user input
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := l.http.Do(req) //nolint:gosec // G704: configured LiveKit address, not user input
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Code == "not_found" {
			return ErrNotFound
		}
		return fmt.Errorf("livekit %s: %d %s %s", method, resp.StatusCode, e.Code, e.Msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ErrBadWebhook: the request is not a webhook signed by our LiveKit.
var ErrBadWebhook = errors.New("livekit: webhook signature does not match")

// maxWebhookBytes bounds a webhook body; events about a room are small.
const maxWebhookBytes = 1 << 20

// VerifyWebhook reads a webhook request and checks that LiveKit sent it:
// the Authorization header is a token signed with our secret whose sha256
// claim is the base64 SHA-256 of the body.
func (l *LiveKit) VerifyWebhook(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxWebhookBytes {
		return nil, ErrBadWebhook
	}
	var c claims
	_, err = jwt.ParseWithClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &c,
		func(*jwt.Token) (any, error) { return []byte(l.secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(l.key),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(l.now),
	)
	if err != nil || l.secret == "" {
		return nil, ErrBadWebhook
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(c.Sha256), []byte(base64.StdEncoding.EncodeToString(sum[:]))) != 1 {
		return nil, ErrBadWebhook
	}
	return body, nil
}
