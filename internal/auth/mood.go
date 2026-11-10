package auth

import (
	"slices"
	"strings"
	"time"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

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

const (
	moodTextMax = 80
	// moodMaxTTL is how far ahead a mood may expire.
	moodMaxTTL = 7 * 24 * time.Hour
	// moodClockSlack lets a client whose clock runs ahead of ours ask for
	// the full moodMaxTTL (docs/adr/0046-mood.md).
	moodClockSlack = 5 * time.Minute
)

// MoodInput is the mood in the body of PATCH /users/@me.
type MoodInput struct {
	State     string     `json:"state"`
	Text      string     `json:"text"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// check trims the text and reports what is wrong with the mood under the
// "mood" key of fields.
func (m *MoodInput) check(fields httpx.Fields, now time.Time) {
	m.Text = strings.TrimSpace(m.Text)
	if m.ExpiresAt != nil {
		// Postgres keeps microseconds; the stored value must compare equal.
		at := m.ExpiresAt.UTC().Truncate(time.Microsecond)
		m.ExpiresAt = &at
	}
	switch {
	case !slices.Contains(MoodStates, m.State):
		fields["mood"] = "Одно из: " + strings.Join(MoodStates, ", ")
	case m.ExpiresAt != nil && (!m.ExpiresAt.After(now) || m.ExpiresAt.After(now.Add(moodMaxTTL+moodClockSlack))):
		fields["mood"] = "Срок — в будущем и не дальше 7 дней"
	default:
		fields.Length("mood", m.Text, 0, moodTextMax)
	}
}

// moodRow is the mood as the users row keeps it; no state means no mood.
type moodRow struct {
	state   *string
	text    string
	expires *time.Time
}

// row is the stored form of the input; nil clears the mood.
func (m *MoodInput) row() moodRow {
	if m == nil {
		return moodRow{}
	}
	return moodRow{state: &m.State, text: m.Text, expires: m.ExpiresAt}
}

func (a moodRow) equal(b moodRow) bool {
	if (a.state == nil) != (b.state == nil) || (a.expires == nil) != (b.expires == nil) {
		return false
	}
	return (a.state == nil || *a.state == *b.state) && a.text == b.text &&
		(a.expires == nil || a.expires.Equal(*b.expires))
}
