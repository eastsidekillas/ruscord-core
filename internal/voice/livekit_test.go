package voice

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testKey    = "APIkey"
	testSecret = "a-secret-of-at-least-thirty-two-chars"
)

func newTestLiveKit(url string) (*LiveKit, time.Time) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	lk := NewLiveKit(url, testKey, testSecret)
	lk.now = func() time.Time { return now }
	return lk, now
}

func parse(t *testing.T, token, secret string, now time.Time) claims {
	t.Helper()
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestJoinToken(t *testing.T) {
	lk, now := newTestLiveKit("ws://livekit:7880")
	for _, c := range []struct {
		name    string
		sources []string
		publish bool
	}{
		{"speaker with a screen", []string{"microphone", "screen_share", "screen_share_audio"}, true},
		{"listener", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tok, err := lk.JoinToken(Join{Room: "voice:r", Identity: "u1", Name: "Аня", Metadata: `{"avatar":null}`,
				Sources: c.sources, TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			cl := parse(t, tok, testSecret, now)
			v := cl.Video
			switch {
			case cl.Issuer != testKey || cl.Subject != "u1" || cl.Name != "Аня" || cl.Metadata != `{"avatar":null}`:
				t.Fatalf("claims %+v", cl)
			case !cl.ExpiresAt.Equal(now.Add(time.Minute)):
				t.Fatalf("exp %v", cl.ExpiresAt)
			case v == nil || !v.RoomJoin || v.Room != "voice:r" || v.RoomAdmin || v.RoomList:
				t.Fatalf("video grant %+v: join only, never admin", v)
			case *v.CanPublish != c.publish || !slices.Equal(v.CanPublishSources, c.sources):
				t.Fatalf("publish %v %v", *v.CanPublish, v.CanPublishSources)
			case !*v.CanSubscribe || *v.CanPublishData || *v.CanUpdateOwnMetadata:
				t.Fatalf("subscribe %v, data %v, own metadata %v", *v.CanSubscribe, *v.CanPublishData, *v.CanUpdateOwnMetadata)
			}
		})
	}
	if _, err := NewLiveKit("ws://x", "", "").JoinToken(Join{Room: "r"}); err == nil {
		t.Fatal("a token without keys")
	}
}

// signedWebhook builds a request the way livekit/protocol's notifier does.
func signedWebhook(t *testing.T, body, key, secret string, sum string, at time.Time) *http.Request {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256([]byte(body))
		sum = base64.StdEncoding.EncodeToString(h[:])
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: key, NotBefore: jwt.NewNumericDate(at), ExpiresAt: jwt.NewNumericDate(at.Add(5 * time.Minute))},
		Sha256:           sum,
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, WebhookPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/webhook+json")
	r.Header.Set("Authorization", tok)
	return r
}

func TestVerifyWebhook(t *testing.T) {
	lk, now := newTestLiveKit("ws://livekit:7880")
	body := `{"event":"participant_joined"}`
	for _, c := range []struct {
		name string
		req  func() *http.Request
		ok   bool
	}{
		{"signed by our LiveKit", func() *http.Request { return signedWebhook(t, body, testKey, testSecret, "", now) }, true},
		{"another secret", func() *http.Request {
			return signedWebhook(t, body, testKey, "another-secret-of-thirty-two-chars!!", "", now)
		}, false},
		{"another key", func() *http.Request { return signedWebhook(t, body, "other", testSecret, "", now) }, false},
		{"body changed", func() *http.Request {
			r := signedWebhook(t, body, testKey, testSecret, "", now)
			r.Body = io.NopCloser(strings.NewReader(`{"event":"participant_left"}`))
			return r
		}, false},
		{"no checksum", func() *http.Request { return signedWebhook(t, body, testKey, testSecret, "-", now) }, false},
		{"expired", func() *http.Request { return signedWebhook(t, body, testKey, testSecret, "", now.Add(-time.Hour)) }, false},
		{"no header", func() *http.Request {
			r := signedWebhook(t, body, testKey, testSecret, "", now)
			r.Header.Del("Authorization")
			return r
		}, false},
		{"alg none", func() *http.Request {
			r := signedWebhook(t, body, testKey, testSecret, "", now)
			tok, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": testKey}).SignedString(jwt.UnsafeAllowNoneSignatureType)
			r.Header.Set("Authorization", tok)
			return r
		}, false},
		{"too large", func() *http.Request {
			big := `{"x":"` + strings.Repeat("a", maxWebhookBytes) + `"}`
			return signedWebhook(t, big, testKey, testSecret, "", now)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := lk.VerifyWebhook(c.req())
			if c.ok && (err != nil || string(got) != body) {
				t.Fatalf("rejected: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// fakeRoomService answers RoomService calls like LiveKit and records them.
type fakeRoomService struct {
	t     *testing.T
	calls []string
	rooms map[string][]Participant
}

func (f *fakeRoomService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/twirp/livekit.RoomService/")
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var c claims
	if _, err := jwt.ParseWithClaims(tok, &c, func(*jwt.Token) (any, error) { return []byte(testSecret), nil }); err != nil || c.Video == nil {
		http.Error(w, `{"code":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}
	needAdmin := method != "ListRooms"
	if needAdmin && (!c.Video.RoomAdmin || c.Video.Room != in["room"]) || !needAdmin && !c.Video.RoomList {
		http.Error(w, `{"code":"permission_denied"}`, http.StatusForbidden)
		return
	}
	f.calls = append(f.calls, method+" "+in["room"]+" "+in["identity"])
	switch method {
	case "ListRooms":
		var rooms []map[string]string
		for name := range f.rooms {
			rooms = append(rooms, map[string]string{"name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rooms": rooms})
	case "ListParticipants":
		ps, ok := f.rooms[in["room"]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","msg":"room not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"participants": ps})
	case "RemoveParticipant":
		ps := f.rooms[in["room"]]
		i := slices.IndexFunc(ps, func(p Participant) bool { return p.Identity == in["identity"] })
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","msg":"participant not found"}`))
			return
		}
		f.rooms[in["room"]] = slices.Delete(ps, i, i+1)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.Error(w, `{"code":"bad_route"}`, http.StatusNotFound)
	}
}

func TestRoomService(t *testing.T) {
	f := &fakeRoomService{t: t, rooms: map[string][]Participant{"voice:a": {{Sid: "PA_1", Identity: "u1"}}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	lk := NewLiveKit(strings.Replace(srv.URL, "http://", "ws://", 1), testKey, testSecret)
	ctx := t.Context()

	if names, err := lk.ListRooms(ctx); err != nil || !slices.Equal(names, []string{"voice:a"}) {
		t.Fatalf("rooms %v %v", names, err)
	}
	if ps, err := lk.ListParticipants(ctx, "voice:a"); err != nil || len(ps) != 1 || ps[0].Sid != "PA_1" {
		t.Fatalf("participants %v %v", ps, err)
	}
	if ps, err := lk.ListParticipants(ctx, "voice:gone"); err != nil || ps != nil {
		t.Fatalf("a room that is not there: %v %v", ps, err)
	}
	if err := lk.RemoveParticipant(ctx, "voice:a", "u1"); err != nil || len(f.rooms["voice:a"]) != 0 {
		t.Fatalf("remove: %v", err)
	}
	if err := lk.RemoveParticipant(ctx, "voice:a", "u1"); err != nil {
		t.Fatalf("removing someone not there: %v", err)
	}
	wrong := NewLiveKit(srv.URL, testKey, "wrong-secret-of-thirty-two-characters")
	if _, err := wrong.ListParticipants(ctx, "voice:a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a bad token must fail loudly: %v", err)
	}
}
