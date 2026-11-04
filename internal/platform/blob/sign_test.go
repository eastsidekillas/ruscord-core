package blob

import (
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSigner(t *testing.T) {
	s := NewSigner([]byte("secret"))
	// The start of a window: links made within it are the same.
	now := time.Unix(1_800_000_000/43200*43200, 0)
	s.now = func() time.Time { return now }
	key := AttachmentKey(uuid.New(), uuid.New(), ExtPNG)
	link, _ := url.Parse(s.URL(key, "кот.png"))
	if link.Path != "/cdn/"+key {
		t.Fatalf("path = %s", link.Path)
	}
	q := link.Query()
	if !s.Valid(key, q) {
		t.Fatal("a fresh link must be valid")
	}
	// The same link for a whole window, so browsers can cache it.
	s.now = func() time.Time { return now.Add(time.Hour) }
	if again, _ := url.Parse(s.URL(key, "кот.png")); again.RawQuery != link.RawQuery {
		t.Fatalf("link changed within its window: %s vs %s", again.RawQuery, link.RawQuery)
	}
	// It lives at least a day and expires after at most a day and a half.
	s.now = func() time.Time { return now.Add(24 * time.Hour) }
	if !s.Valid(key, q) {
		t.Fatal("a link must live a day")
	}
	s.now = func() time.Time { return now.Add(36*time.Hour + time.Second) }
	if s.Valid(key, q) {
		t.Fatal("a link must expire after a day and a half")
	}
	s.now = func() time.Time { return now }
	other := AttachmentKey(uuid.New(), uuid.New(), ExtPNG)
	for name, v := range map[string]url.Values{
		"another key":    nil,
		"another name":   {"name": {"другое.png"}, "ex": q["ex"], "sig": q["sig"]},
		"later expiry":   {"name": q["name"], "ex": {"ffffffffff"}, "sig": q["sig"]},
		"no signature":   {"name": q["name"], "ex": q["ex"]},
		"another secret": nil,
	} {
		k, signer := key, s
		switch name {
		case "another key":
			k, v = other, q
		case "another secret":
			signer, v = NewSigner([]byte("other")), q
			signer.now = s.now
		}
		if signer.Valid(k, v) {
			t.Errorf("%s: valid", name)
		}
	}
}

func TestAttachmentKeys(t *testing.T) {
	ch, id := uuid.New(), uuid.New()
	for _, ext := range []string{ExtPNG, ExtJPEG, ExtGIF, ExtWebP, ExtMP4, ExtWebM, ExtBin} {
		if k := AttachmentKey(ch, id, ext); !ValidKey(k) || Ext(k) != ext || ContentType(ext) == "" {
			t.Errorf("%s: key %s", ext, k)
		}
	}
	for _, bad := range []string{
		"attachments/" + ch.String() + "/" + id.String() + ".svg",
		"attachments/" + ch.String() + "/" + id.String(),
		"attachments/" + ch.String() + "/../" + id.String() + ".png",
		"attachments/" + ch.String() + "/" + id.String() + ".png/x",
	} {
		if ValidKey(bad) {
			t.Errorf("ValidKey(%q) = true", bad)
		}
	}
}

func TestMemoryRanges(t *testing.T) {
	for rng, want := range map[string]string{
		"bytes=0-3": "0123", "bytes=6-": "6789", "bytes=-2": "89", "bytes=8-100": "89", "bytes=-100": "0123456789",
	} {
		start, end, ok := parseRange(rng, 10)
		if got := "0123456789"[start : end+1]; !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", rng, got, ok, want)
		}
	}
	for _, bad := range []string{"bytes=10-", "bytes=5-2", "items=0-1", "bytes=0-1,3-4", "bytes=-0", "bytes=a-b", "0-1"} {
		if _, _, ok := parseRange(bad, 10); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}
