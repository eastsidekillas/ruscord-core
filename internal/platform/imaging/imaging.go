// Package imaging turns an uploaded picture into a safe square image
// (docs/adr/0019-files.md).
//
// Uploads are hostile input: the format is detected from the content, the
// declared size is checked before any pixel is decoded (a few kilobytes can
// declare gigapixels), and the result is always re-encoded, which drops
// EXIF (location) and anything a polyglot file carried besides pixels.
package imaging

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	// Decoders: the formats accepted, detected by their magic numbers.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
)

// Limits.
const (
	MaxUploadBytes = 8 << 20
	MaxSide        = 4096 // per dimension, checked before decoding
	Size           = 256  // side of the produced square
	jpegQuality    = 85
)

var (
	// ErrFormat: not an image in a supported raster format (SVG included).
	ErrFormat = errors.New("imaging: unsupported or invalid image")
	// ErrTooLarge: wider or taller than MaxSide.
	ErrTooLarge = errors.New("imaging: image is too large")
)

// slots bounds concurrent decodes: one MaxSide² image takes 64 MiB as RGBA.
var slots = make(chan struct{}, 4)

// Square crops the centre square of an image and scales it to Size×Size.
// The result is PNG if the picture has any transparency, JPEG otherwise;
// the returned extension is blob.ExtPNG or blob.ExtJPEG.
func Square(ctx context.Context, data []byte) ([]byte, string, error) {
	if len(data) == 0 || len(data) > MaxUploadBytes {
		return nil, "", ErrFormat
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrFormat
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", ErrFormat
	}
	if cfg.Width > MaxSide || cfg.Height > MaxSide {
		return nil, "", ErrTooLarge
	}

	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-slots }()

	src, _, err := image.Decode(bytes.NewReader(data)) // GIF: the first frame
	if err != nil {
		return nil, "", ErrFormat
	}
	b := src.Bounds()
	if b.Dx() > MaxSide || b.Dy() > MaxSide { // the header lied
		return nil, "", ErrTooLarge
	}
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(0, 0, side, side).Add(image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2))
	dst := image.NewNRGBA(image.Rect(0, 0, Size, Size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)

	var out bytes.Buffer
	if opaque(dst) {
		err = jpeg.Encode(&out, flatten(dst), &jpeg.Options{Quality: jpegQuality})
		return out.Bytes(), blob.ExtJPEG, err
	}
	err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, dst)
	return out.Bytes(), blob.ExtPNG, err
}

func opaque(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return false
		}
	}
	return true
}

// flatten drops the alpha channel the JPEG encoder would otherwise read.
func flatten(img *image.NRGBA) image.Image {
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(rgba, rgba.Bounds(), img, image.Point{}, draw.Over)
	return rgba
}
