package imaging

import (
	"image"
	"io"
	"net/http"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
)

// MaxAttachmentSide: a bigger picture is attached as a file to download,
// not shown in the page.
const MaxAttachmentSide = 8192

// Sniffed is what an attachment turned out to be by its content
// (docs/adr/0036-attachments.md).
type Sniffed struct {
	// Ext is the extension of its key: a format shown in the page, or
	// blob.ExtBin for anything that is only downloaded.
	Ext         string
	ContentType string
	// Width and Height of a picture; 0 otherwise.
	Width, Height int
}

var shown = map[string]string{
	"image/png": blob.ExtPNG, "image/jpeg": blob.ExtJPEG, "image/gif": blob.ExtGIF, "image/webp": blob.ExtWebP,
	"video/mp4": blob.ExtMP4, "video/webm": blob.ExtWebM,
}

// Sniff detects the format of a file from its first bytes, never from its
// name or the type the client claims. Pictures are shown only if their
// declared size can be read and is not too large; nothing is decoded
// beyond the header. Anything else, SVG and HTML included, is a download.
func Sniff(r io.ReaderAt) Sniffed {
	head := make([]byte, 512)
	n, _ := r.ReadAt(head, 0)
	typ := http.DetectContentType(head[:n])
	ext, ok := shown[typ]
	if !ok {
		return Sniffed{Ext: blob.ExtBin, ContentType: typ}
	}
	if typ[:6] != "image/" {
		return Sniffed{Ext: ext, ContentType: typ}
	}
	cfg, _, err := image.DecodeConfig(io.NewSectionReader(r, 0, 1<<62))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxAttachmentSide || cfg.Height > MaxAttachmentSide {
		return Sniffed{Ext: blob.ExtBin, ContentType: typ}
	}
	return Sniffed{Ext: ext, ContentType: typ, Width: cfg.Width, Height: cfg.Height}
}
