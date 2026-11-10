package auth

import "time"

// Mood is the short status a user shows on their profile.
type Mood struct {
	State     string     `json:"state"`
	Text      string     `json:"text"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// MoodStates are the allowed values of Mood.State.
var MoodStates = []string{"happy", "laughing", "thinking", "sleeping", "listening", "surprised", "confused", "angry"}

// ProfileColors are the allowed profile colours; the first is the default.
var ProfileColors = []string{"accent", "success", "warning", "danger", "neutral"}

// liveMood builds the mood from its columns. It returns nil without a state
// or when the mood has expired (expires <= now): expiry is applied on read,
// no job clears the columns.
func liveMood(state *string, text string, expires *time.Time, now time.Time) *Mood {
	if state == nil || (expires != nil && !expires.After(now)) {
		return nil
	}
	return &Mood{State: *state, Text: text, ExpiresAt: expires}
}
