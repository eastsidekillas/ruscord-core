package message

import (
	"errors"
	"image/color"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

func files(ch string) string { return "/api/v1/channels/" + ch + "/attachments" }

// upload sends a file and returns the attachment the server made.
func upload(t *testing.T, u *apitest.User, ch, name string, data []byte) map[string]any {
	t.Helper()
	code, out := u.Upload("POST", files(ch)+"?filename="+url.QueryEscape(name), data)
	if code != http.StatusCreated {
		t.Fatalf("upload %s: %d %v", name, code, out)
	}
	return obj(out)
}

func TestCleanFilename(t *testing.T) {
	for in, want := range map[string]string{
		"кот.png": "кот.png", "../../etc/passwd": "passwd", `C:\Users\a\отчёт.pdf`: "отчёт.pdf",
		"  a\x00b\tc.txt ": "abc.txt", strings.Repeat("я", maxFilename): strings.Repeat("я", maxFilename),
	} {
		if got, err := cleanFilename(in); err != nil || got != want {
			t.Errorf("cleanFilename(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "/", "..", "\x00", strings.Repeat("я", maxFilename+1)} {
		if got, err := cleanFilename(bad); err == nil {
			t.Errorf("cleanFilename(%q) = %q, want an error", bad, got)
		}
	}
}

func TestAttachments(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, text, voice := e.server(owner, bob)
	pic := apitest.PNG(40, 20, color.RGBA{R: 200, A: 255})

	// What the server sniffs, not what the name says.
	img := upload(t, bob, text, "кот.jpg", pic)
	if img["content_type"] != "image/png" || img["width"] != float64(40) || img["height"] != float64(20) || img["size"] != float64(len(pic)) {
		t.Fatalf("picture = %v", img)
	}
	if _, ok := img["url"]; ok {
		t.Fatalf("an upload has no link until it is sent: %v", img)
	}
	doc := upload(t, bob, text, "../../notes.txt", []byte("привет, это заметка"))
	if doc["filename"] != "notes.txt" || !strings.HasPrefix(str(doc["content_type"]), "text/plain") || doc["width"] != nil {
		t.Fatalf("text = %v", doc)
	}
	svg := upload(t, bob, text, "x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	// Who may upload, and what.
	if code, _ := eve.Upload("POST", files(text)+"?filename=a.png", pic); code != http.StatusNotFound {
		t.Fatalf("outsider: %d", code)
	}
	if code, _ := bob.Upload("POST", files(voice)+"?filename=a", pic); code != http.StatusBadRequest {
		t.Fatalf("upload to a voice channel: %d", code)
	}
	if code, _ := bob.Upload("POST", files(text)+"?filename=a", nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty file: %d", code)
	}
	if code, _ := bob.Upload("POST", files(text), pic); code != http.StatusUnprocessableEntity {
		t.Fatalf("no filename: %d", code)
	}
	// The size is checked before the body is read.
	if _, err := e.messages.Upload(t.Context(), bob.ID, uuid.MustParse(text), "big.bin", strings.NewReader(""), MaxAttachmentBytes+1); !errors.Is(err, errFileTooBig) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := e.messages.Upload(t.Context(), bob.ID, uuid.MustParse(text), "a.bin", strings.NewReader(""), -1); !errors.Is(err, errNoLength) {
		t.Fatalf("no length: %v", err)
	}
	ow := "/api/v1/channels/" + text + "/permissions/" + gid
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.AttachFiles)})
	if code, _ := bob.Upload("POST", files(text)+"?filename=a.png", pic); code != http.StatusForbidden {
		t.Fatalf("without ATTACH_FILES: %d", code)
	}
	owner.Expect(http.StatusNoContent, "DELETE", ow, nil)

	// Sending: files without text; only one's own fresh uploads in this channel.
	ids := []any{img["id"], doc["id"], svg["id"]}
	m := obj(bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "", "attachments": ids}))
	got := list(m["attachments"])
	if m["content"] != "" || len(got) != 3 || obj(got[0])["id"] != img["id"] || obj(got[2])["id"] != svg["id"] {
		t.Fatalf("message = %v", m)
	}
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "снова", "attachments": []any{img["id"]}})
	mine := upload(t, owner, text, "a.png", pic)
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "чужой", "attachments": []any{mine["id"]}})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "", "attachments": []any{}})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "x", "attachments": []any{mine["id"], mine["id"]}})
	many := make([]any, maxAttachments+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "x", "attachments": many})
	if n := len(list(obj(list(owner.Expect(http.StatusOK, "GET", msgs(text)+"?limit=1", nil))[0])["attachments"])); n != 3 {
		t.Fatalf("history shows %d attachments", n)
	}

	// Links: signed, inert, pictures inline and everything else a download.
	link := func(i int) string { return str(obj(got[i])["url"]) }
	code, h, body := e.FetchRange(link(0), "")
	if code != http.StatusOK || h.Get("Content-Type") != "image/png" || string(body) != string(pic) ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "inline") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("picture: %d %v", code, h)
	}
	for _, i := range []int{1, 2} {
		code, h, _ := e.FetchRange(link(i), "")
		if code != http.StatusOK || h.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Fatalf("file %d: %d %v", i, code, h)
		}
	}
	if _, h, _ := e.FetchRange(link(1), ""); !strings.Contains(h.Get("Content-Disposition"), "notes.txt") {
		t.Fatalf("download name: %v", h.Get("Content-Disposition"))
	}
	// A range, for seeking in videos.
	code, h, body = e.FetchRange(link(0), "bytes=1-3")
	if code != http.StatusPartialContent || string(body) != string(pic[1:4]) || h.Get("Content-Range") != "bytes 1-3/"+strconv.Itoa(len(pic)) {
		t.Fatalf("range: %d %v %q", code, h, body)
	}
	if code, _, _ := e.FetchRange(link(0), "bytes=99999-"); code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range outside: %d", code)
	}
	// Without a valid signature the file does not exist.
	u, _ := url.Parse(link(0))
	q := u.Query()
	for _, broken := range []url.Values{
		{"name": q["name"], "ex": q["ex"]},
		{"name": {"другое.png"}, "ex": q["ex"], "sig": q["sig"]},
		{"name": q["name"], "ex": {"1"}, "sig": q["sig"]},
	} {
		if code, _ := e.Fetch(u.Path + "?" + broken.Encode()); code != http.StatusNotFound {
			t.Fatalf("broken link %v: %d", broken, code)
		}
	}
	if code, _ := e.Fetch(u.Path); code != http.StatusNotFound {
		t.Fatalf("unsigned link: %d", code)
	}

	// A message with files may lose its text; one without may not.
	bob.Expect(http.StatusOK, "PATCH", msgs(text)+"/"+str(m["id"]), map[string]any{"content": ""})
	plain := send(bob, text, "только текст")
	bob.Expect(http.StatusUnprocessableEntity, "PATCH", msgs(text)+"/"+str(plain["id"]), map[string]any{"content": ""})

	// Deleting the message schedules its files for a day; then they go.
	bob.Expect(http.StatusNoContent, "DELETE", msgs(text)+"/"+str(m["id"]), nil)
	key := strings.TrimPrefix(u.Path, "/cdn/")
	if h := e.Garbage()[key]; h != 24 {
		t.Fatalf("file of a deleted message due in %dh: %v", h, e.Garbage())
	}
	// The owner's upload was never sent: once too old to attach, it goes
	// with its row. (Sweep makes files due at once; for real, an upload's
	// file is due an hour after it came.)
	if _, err := e.PG.Exec(t.Context(), `UPDATE attachments SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	e.Sweep()
	if code, _ := e.Fetch(link(0)); code != http.StatusNotFound {
		t.Fatalf("file after its message was deleted: %d", code)
	}
	var left int
	_ = e.PG.QueryRow(t.Context(), `SELECT count(*) FROM attachments`).Scan(&left)
	if left != 0 || len(e.Blobs.Keys()) != 0 {
		t.Fatalf("left: %d rows, files %v", left, e.Blobs.Keys())
	}

	// Deleting the channel takes its files, pending uploads included.
	sent := upload(t, bob, text, "a.png", pic)
	bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "x", "attachments": []any{sent["id"]}})
	upload(t, bob, text, "b.png", pic)
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+text, nil)
	e.Sweep()
	if len(e.Blobs.Keys()) != 0 {
		t.Fatalf("files of a deleted channel: %v", e.Blobs.Keys())
	}
}

func TestAttachmentsInDM(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")
	dm := e.dm(ann, bob)
	a := upload(t, ann, dm, "фото.png", apitest.PNG(8, 8, color.White))
	m := obj(ann.Expect(http.StatusCreated, "POST", msgs(dm), map[string]any{"content": "смотри", "attachments": []any{a["id"]}}))
	if len(list(m["attachments"])) != 1 {
		t.Fatalf("DM message = %v", m)
	}
	if code, _ := eve.Upload("POST", files(dm)+"?filename=a.png", apitest.PNG(8, 8, color.Black)); code != http.StatusNotFound {
		t.Fatalf("outsider: %d", code)
	}
	// After a block nobody in the DM may send, files included.
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{"type": 2})
	if code, _ := ann.Upload("POST", files(dm)+"?filename=a.png", apitest.PNG(8, 8, color.Black)); code != http.StatusForbidden {
		t.Fatalf("upload after a block: %d", code)
	}
}
