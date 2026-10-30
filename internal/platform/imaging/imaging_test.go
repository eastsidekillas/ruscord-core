package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// Fixtures in testdata are made with Pillow (see the devlog of step 5b-2).

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pngHeader is a PNG that declares w×h in a valid IHDR and has no pixels:
// the size check must refuse it before any decoding.
func pngHeader(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	const ihdrLen = 13
	ihdr := make([]byte, ihdrLen)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&buf, binary.BigEndian, uint32(ihdrLen))
	buf.Write(chunk)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSquare(t *testing.T) {
	polyglot := append(fixture(t, "alpha.png"), []byte("<script>alert('POLYGLOT-PAYLOAD')</script>")...)
	cases := []struct {
		name    string
		data    []byte
		ext     string
		err     error
		without string // must not appear in the output
	}{
		{name: "photo JPEG with EXIF and GPS", data: fixture(t, "photo.jpg"), ext: blob.ExtJPEG, without: "SECRET-EXIF-MARKER"},
		{name: "transparent PNG", data: fixture(t, "alpha.png"), ext: blob.ExtPNG},
		{name: "animated GIF, first frame", data: fixture(t, "anim.gif"), ext: blob.ExtJPEG},
		{name: "lossy WebP", data: fixture(t, "photo.webp"), ext: blob.ExtJPEG},
		{name: "lossless WebP with alpha", data: fixture(t, "alpha.webp"), ext: blob.ExtPNG},
		{name: "tiny image is scaled up", data: fixture(t, "tiny.png"), ext: blob.ExtJPEG},
		{name: "PNG with a payload after the image", data: polyglot, ext: blob.ExtPNG, without: "POLYGLOT-PAYLOAD"},
		{name: "exactly the largest side", data: encodePNG(t, image.NewGray(image.Rect(0, 0, MaxSide, 1))), ext: blob.ExtJPEG},

		{name: "animated WebP", data: fixture(t, "anim.webp"), err: ErrFormat},
		{name: "SVG", data: []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect width="10" height="10"/></svg>`), err: ErrFormat},
		{name: "HTML", data: []byte("<!doctype html><script>alert(1)</script>"), err: ErrFormat},
		{name: "empty", data: nil, err: ErrFormat},
		{name: "truncated JPEG", data: fixture(t, "photo.jpg")[:200], err: ErrFormat},
		{name: "header only, no pixels", data: pngHeader(64, 64), err: ErrFormat},
		{name: "declares 100000x100000", data: pngHeader(100000, 100000), err: ErrTooLarge},
		{name: "one side too long", data: pngHeader(MaxSide+1, 10), err: ErrTooLarge},
		{name: "zero width", data: pngHeader(0, 10), err: ErrFormat},
		{name: "over the byte limit", data: make([]byte, MaxUploadBytes+1), err: ErrFormat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ext, err := Square(t.Context(), c.data)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("err = %v, want %v", err, c.err)
				}
				return
			}
			if err != nil || ext != c.ext {
				t.Fatalf("ext %q err %v, want %q", ext, err, c.ext)
			}
			img, format, err := image.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("output does not decode: %v", err)
			}
			if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
				t.Fatalf("output is %v, want %dx%d", b, Size, Size)
			}
			if want := map[string]string{blob.ExtPNG: "png", blob.ExtJPEG: "jpeg"}[ext]; format != want {
				t.Fatalf("output format %s, extension %s", format, ext)
			}
			if c.without != "" && bytes.Contains(out, []byte(c.without)) {
				t.Fatalf("output still carries %q", c.without)
			}
			if bytes.Contains(out, []byte("Exif")) {
				t.Fatal("output carries an EXIF block")
			}
		})
	}
}

// The centre of a wide image is kept: a red square in the middle of white
// margins comes out red in the middle.
func TestSquareCropsCentre(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 100))
	for x := range 300 {
		for y := range 100 {
			c := color.RGBA{255, 255, 255, 255}
			if x >= 100 && x < 200 {
				c = color.RGBA{255, 0, 0, 255}
			}
			src.Set(x, y, c)
		}
	}
	out, _, err := Square(t.Context(), encodePNG(t, src))
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := image.Decode(bytes.NewReader(out))
	r, g, b, _ := img.At(Size/2, Size/2).RGBA()
	if r>>8 < 200 || g>>8 > 60 || b>>8 > 60 {
		t.Fatalf("centre pixel %d,%d,%d, want red", r>>8, g>>8, b>>8)
	}
	r, g, b, _ = img.At(2, 2).RGBA()
	if r>>8 < 200 || g>>8 > 60 {
		t.Fatalf("corner %d,%d,%d: the white margins must be cropped away", r>>8, g>>8, b>>8)
	}
}

func TestSquareWaitsForASlot(t *testing.T) {
	for range cap(slots) {
		slots <- struct{}{}
	}
	defer func() {
		for range cap(slots) {
			<-slots
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Square(ctx, fixture(t, "alpha.png")); !errors.Is(err, context.Canceled) {
		t.Fatalf("with every slot busy and the request gone: %v", err)
	}
}

func TestStoreWithoutFiles(t *testing.T) {
	_, err := Store(t.Context(), nil, blob.KindAvatar, [16]byte{}, fixture(t, "alpha.png"))
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Fatalf("without storage: %v, want 503", err)
	}
}
