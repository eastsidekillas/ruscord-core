package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Kinds of stored files; the first path segment of a key.
const (
	KindAvatar     = "avatars"
	KindIcon       = "icons"
	KindAttachment = "attachments"
)

// Formats of stored files. The image pipeline produces PNG and JPEG; an
// attachment keeps the format it was sniffed as, or bin for anything that
// is not shown in the page.
const (
	ExtPNG  = "png"
	ExtJPEG = "jpg"
	ExtGIF  = "gif"
	ExtWebP = "webp"
	ExtMP4  = "mp4"
	ExtWebM = "webm"
	ExtBin  = "bin"
)

var contentTypes = map[string]string{
	ExtPNG: "image/png", ExtJPEG: "image/jpeg", ExtGIF: "image/gif", ExtWebP: "image/webp",
	ExtMP4: "video/mp4", ExtWebM: "video/webm", ExtBin: "application/octet-stream",
}

// ContentType of a file stored with extension ext.
func ContentType(ext string) string { return contentTypes[ext] }

// Ext is the extension of a key.
func Ext(key string) string { return key[strings.LastIndexByte(key, '.')+1:] }

// AttachmentKey names an attachment: "attachments/<channel>/<id>.<ext>".
// The id is new for every upload, so the key is never reused.
func AttachmentKey(channelID, id uuid.UUID, ext string) string {
	return KindAttachment + "/" + channelID.String() + "/" + id.String() + "." + ext
}

// Key names a file by what it is, whose it is and what it contains:
// "avatars/<owner>/<sha256>.png". The same content gives the same key, and a
// key is never reused for other content, so objects are immutable.
func Key(kind string, owner uuid.UUID, content []byte, ext string) string {
	sum := sha256.Sum256(content)
	return kind + "/" + owner.String() + "/" + hex.EncodeToString(sum[:]) + "." + ext
}

const uuidRe = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var keyRe = regexp.MustCompile(`^(?:(?:avatars|icons)/` + uuidRe + `/[0-9a-f]{64}\.(?:png|jpg)` +
	`|attachments/` + uuidRe + `/` + uuidRe + `\.(?:png|jpg|gif|webp|mp4|webm|bin))$`)

// ValidKey reports whether key is one this package could have produced.
// Anything else (traversal, other buckets' paths, other formats) is refused
// before it reaches the store.
func ValidKey(key string) bool { return keyRe.MatchString(key) }

// URL is the path clients load a file from, relative to the API origin.
func URL(key string) string { return "/cdn/" + key }

// URLPtr is URL for an optional key; nil stays nil.
// SameKey reports whether two optional keys are equal (both absent counts).
func SameKey(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func URLPtr(key *string) *string {
	if key == nil {
		return nil
	}
	u := URL(*key)
	return &u
}
