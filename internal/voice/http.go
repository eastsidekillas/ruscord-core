package voice

import (
	"errors"
	"net/http"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// Register adds the client routes (API role).
func (s *Service) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/channels/{channel}/voice", requireAuth(httpx.Handle(s.join)))
	mux.Handle("PATCH /api/v1/channels/{channel}/voice", requireAuth(httpx.Handle(s.selfVoice)))
}

// WebhookPath is where LiveKit posts its webhooks. It is outside /api, so
// the client's nginx does not expose it; LiveKit reaches the core directly.
const WebhookPath = "/livekit/webhook"

// RegisterWebhook adds the LiveKit webhook route (voice role).
func (s *Service) RegisterWebhook(mux *http.ServeMux, lk *LiveKit) {
	mux.HandleFunc("POST "+WebhookPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := lk.VerifyWebhook(r)
		if err != nil {
			if !errors.Is(err, ErrBadWebhook) {
				s.log.WarnContext(r.Context(), "voice: webhook read", "err", err)
			}
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if err := s.Webhook(r.Context(), body); err != nil {
			// LiveKit retries a failed delivery a few times.
			s.log.ErrorContext(r.Context(), "voice: webhook", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Service) join(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	info, err := s.Join(r.Context(), auth.UserID(r), ch)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, info)
	return nil
}

type selfVoiceInput struct {
	SelfMute bool `json:"self_mute"`
	SelfDeaf bool `json:"self_deaf"`
}

func (s *Service) selfVoice(w http.ResponseWriter, r *http.Request) error {
	ch, err := httpx.PathUUID(r, "channel")
	if err != nil {
		return err
	}
	var in selfVoiceInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	st, err := s.SelfVoice(r.Context(), auth.UserID(r), ch, in.SelfMute, in.SelfDeaf)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, st)
	return nil
}
