package imaging

import (
	"bytes"
	"io/fs"
	"os"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
)

func TestSniff(t *testing.T) {
	file := func(name string) []byte {
		b, err := fs.ReadFile(os.DirFS("testdata"), name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	png := file("alpha.png")
	cases := []struct {
		name string
		data []byte
		ext  string
		typ  string
		w    bool
	}{
		{"png", png, blob.ExtPNG, "image/png", true},
		{"jpeg", file("photo.jpg"), blob.ExtJPEG, "image/jpeg", true},
		{"gif", file("anim.gif"), blob.ExtGIF, "image/gif", true},
		{"webp", file("photo.webp"), blob.ExtWebP, "image/webp", true},
		{"mp4", append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 32)...), blob.ExtMP4, "video/mp4", false},
		{"webm", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\xf7\x81\x01\x42\xf2\x81\x04\x42\xf3\x81\x08\x42\x82\x84webm"), blob.ExtWebM, "video/webm", false},
		{"svg is a download", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), blob.ExtBin, "text/xml; charset=utf-8", false},
		{"html is a download", []byte("<!DOCTYPE html><script>alert(1)</script>"), blob.ExtBin, "text/html; charset=utf-8", false},
		{"text", []byte("просто текст"), blob.ExtBin, "text/plain; charset=utf-8", false},
		{"a broken png is a download", png[:20], blob.ExtBin, "image/png", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sniff(bytes.NewReader(c.data))
			if got.Ext != c.ext || got.ContentType != c.typ || (got.Width > 0) != c.w || (got.Height > 0) != c.w {
				t.Fatalf("Sniff = %+v", got)
			}
		})
	}
}
