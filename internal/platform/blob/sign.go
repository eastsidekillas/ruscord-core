package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"
)

// Signed links to attachments (docs/adr/0036-attachments.md): a file of a
// closed channel must not stay readable forever by whoever once saw its
// link. A link carries its expiry and an HMAC of the key, the file name and
// the expiry.
const (
	// signWindow rounds expiries, so a file keeps one link for this long and
	// browsers can cache it.
	signWindow = 12 * time.Hour
	// signWindows is how many windows a fresh link lives: 24 to 36 hours.
	signWindows = 3
	sigBytes    = 16
)

// Signer signs and checks links to attachments with a secret shared by all
// API processes.
type Signer struct {
	secret []byte
	now    func() time.Time
}

func NewSigner(secret []byte) *Signer { return &Signer{secret: secret, now: time.Now} }

// URL is the signed path of an attachment; name is the file name it is
// downloaded as.
func (s *Signer) URL(key, name string) string {
	ex := (s.now().Unix()/int64(signWindow/time.Second) + signWindows) * int64(signWindow/time.Second)
	exs := strconv.FormatInt(ex, 16)
	q := url.Values{"name": {name}, "ex": {exs}, "sig": {s.sig(key, name, exs)}}
	return URL(key) + "?" + q.Encode()
}

// Valid reports whether the query of a link signs key and has not expired.
func (s *Signer) Valid(key string, q url.Values) bool {
	ex, err := strconv.ParseInt(q.Get("ex"), 16, 64)
	if err != nil || s.now().Unix() > ex {
		return false
	}
	want := s.sig(key, q.Get("name"), q.Get("ex"))
	return hmac.Equal([]byte(want), []byte(q.Get("sig")))
}

func (s *Signer) sig(key, name, ex string) string {
	m := hmac.New(sha256.New, s.secret)
	// NUL cannot be in a key, a name (refused at upload) or a hex number.
	m.Write([]byte(key + "\x00" + name + "\x00" + ex))
	return hex.EncodeToString(m.Sum(nil)[:sigBytes])
}
