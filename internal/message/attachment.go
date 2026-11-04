package message

import (
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/imaging"
	"github.com/eastsidekillas/ruscord-core/internal/platform/store"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Limits of attachments (docs/adr/0036-attachments.md).
const (
	MaxAttachmentBytes = 25 << 20
	maxAttachments     = 10
	maxFilename        = 255
	// attachWindow is how long an upload may wait for its message. It is
	// shorter than the collector's grace for fresh uploads, so by the time
	// the collector looks at the file an unattached upload can no longer
	// be attached and goes with its row.
	attachWindow = blob.UploadGrace - 10*time.Minute
)

var (
	ruleUpload      = ratelimit.Rule{Name: "attachment_upload", Limit: 100, Window: time.Hour}
	ruleUploadBytes = ratelimit.Rule{Name: "attachment_bytes", Limit: 1 << 30, Window: time.Hour}

	errNoFiles     = httpx.NewError(http.StatusServiceUnavailable, "files_unavailable", "Загрузка файлов сейчас недоступна")
	errFileTooBig  = httpx.NewError(http.StatusRequestEntityTooLarge, "file_too_large", "Файл больше 25 МиБ")
	errNoLength    = httpx.NewError(http.StatusLengthRequired, "length_required", "Нужен размер файла (Content-Length)")
	errShortBody   = httpx.NewError(http.StatusBadRequest, "invalid_request", "Файл пришёл не целиком")
	errBadFilename = httpx.Validation(map[string]string{"filename": "Имя файла от 1 до 255 символов"})
	errBadAttach   = httpx.Validation(map[string]string{"attachments": "Файл не найден, уже отправлен, загружен в другой канал или больше часа назад"})
	errTooMany     = httpx.Validation(map[string]string{"attachments": "Не больше 10 файлов в сообщении"})
)

// Attachment is a file of a message. URL is a signed link that lives a day
// or more; history and events always carry a fresh one.
type Attachment struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	Width       *int      `json:"width,omitempty"`
	Height      *int      `json:"height,omitempty"`
	URL         string    `json:"url,omitempty"`

	channelID uuid.UUID
	messageID *uuid.UUID
	ext       string
}

func (a Attachment) key() string { return blob.AttachmentKey(a.channelID, a.ID, a.ext) }

// WithFiles turns attachments on: uploads go to files, links are signed by
// signer. Without it uploads answer 503.
func (s *Service) WithFiles(files *blob.Collector, signer *blob.Signer) {
	s.files, s.signer = files, signer
	files.Register(blob.KindAttachment, s.attachmentInUse)
}

// cleanFilename keeps the last path element of a name the client sent and
// drops control characters; what remains is shown and downloaded as is.
func cleanFilename(name string) (string, error) {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, `\`, "/")))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" || utf8.RuneCountInString(name) > maxFilename {
		return "", errBadFilename
	}
	return name, nil
}

// Upload stores a file the user is about to send to a channel. The body is
// spooled to a temporary file first: S3 needs a seekable body to sign, and
// the format is sniffed from the content before anything is stored.
func (s *Service) Upload(ctx context.Context, userID, channelID uuid.UUID, filename string, body io.Reader, size int64) (Attachment, error) {
	if s.files == nil {
		return Attachment{}, errNoFiles
	}
	name, err := cleanFilename(filename)
	if err != nil {
		return Attachment{}, err
	}
	switch {
	case size < 0:
		return Attachment{}, errNoLength
	case size == 0:
		return Attachment{}, httpx.Validation(map[string]string{"file": "Файл пустой"})
	case size > MaxAttachmentBytes:
		return Attachment{}, errFileTooBig
	}
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return Attachment{}, err
	}
	if !ch.perms.Has(perm.SendMessages | perm.AttachFiles) {
		return Attachment{}, httpx.ErrForbidden
	}
	if err := s.limiter.Check(ctx, ruleUpload, userID.String()); err != nil {
		return Attachment{}, err
	}
	if err := s.limiter.CheckN(ctx, ruleUploadBytes, userID.String(), size); err != nil {
		return Attachment{}, err
	}

	f, err := os.CreateTemp("", "ruscord-upload-*")
	if err != nil {
		return Attachment{}, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if n, err := io.Copy(f, io.LimitReader(body, size)); err != nil || n != size {
		return Attachment{}, errShortBody
	}
	kind := imaging.Sniff(f)
	a := Attachment{
		ID: uuid.Must(uuid.NewV7()), Filename: name, Size: size, ContentType: kind.ContentType,
		channelID: channelID, ext: kind.Ext,
	}
	if kind.Width > 0 {
		a.Width, a.Height = &kind.Width, &kind.Height
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Attachment{}, err
	}
	if err := s.files.Upload(ctx, a.key(), f, size); err != nil {
		return Attachment{}, err
	}
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		if err := s.lockChannel(ctx, tx, ch); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO attachments (id, channel_id, uploader_id, filename, size, ext, content_type, width, height)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			a.ID, channelID, userID, a.Filename, a.Size, a.ext, a.ContentType, a.Width, a.Height)
		return err
	})
	return a, err
}

const attachmentCols = `id, channel_id, message_id, filename, size, ext, content_type, width, height`

func scanAttachment(r pgx.CollectableRow) (Attachment, error) {
	var a Attachment
	err := r.Scan(&a.ID, &a.channelID, &a.messageID, &a.Filename, &a.Size, &a.ext, &a.ContentType, &a.Width, &a.Height)
	return a, err
}

// validAttachmentIDs checks the list a message is sent with.
func validAttachmentIDs(ids []uuid.UUID) error {
	if len(ids) > maxAttachments {
		return errTooMany
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			return errBadAttach
		}
		seen[id] = true
	}
	return nil
}

// attach binds the sender's fresh uploads in this channel to a message, in
// the transaction that creates it. Anything else (another's upload, another
// channel, already sent, too old) fails the whole message.
func attach(ctx context.Context, tx pgx.Tx, m *Message, userID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx,
		`UPDATE attachments SET message_id = $1
		 WHERE id = ANY($2) AND channel_id = $3 AND uploader_id = $4 AND message_id IS NULL
		   AND created_at > now() - $5::interval`,
		m.ID, ids, m.ChannelID, userID, interval(attachWindow))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return errBadAttach
	}
	return nil
}

// withAttachments fills in the attachments of messages, with fresh links.
func (s *Service) withAttachments(ctx context.Context, q querier, ms []Message) error {
	ids := make([]uuid.UUID, len(ms))
	for i := range ms {
		ids[i] = ms[i].ID
		ms[i].Attachments = []Attachment{}
	}
	rows, err := q.Query(ctx, `SELECT `+attachmentCols+` FROM attachments WHERE message_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, scanAttachment)
	if err != nil {
		return err
	}
	for _, a := range list {
		i := slices.IndexFunc(ms, func(m Message) bool { return m.ID == *a.messageID })
		if s.signer != nil {
			a.URL = s.signer.URL(a.key(), a.Filename)
		}
		ms[i].Attachments = append(ms[i].Attachments, a)
	}
	return nil
}

// dropAttachments deletes the attachments that match where (with arg) and
// schedules their files for collection in the same transaction: open
// screens keep showing them for a day.
func dropAttachments(ctx context.Context, tx pgx.Tx, where string, arg any) error {
	rows, err := tx.Query(ctx, `DELETE FROM attachments WHERE `+where+` RETURNING `+attachmentCols, arg)
	if err != nil {
		return err
	}
	gone, err := pgx.CollectRows(rows, scanAttachment)
	if err != nil {
		return err
	}
	keys := make([]string, len(gone))
	for i, a := range gone {
		keys[i] = a.key()
	}
	return blob.ScheduleAll(ctx, tx, keys, blob.ReplacedGrace)
}

// attachmentInUse is the blob collector's reference check for attachments.
// It also drops the row of an upload nobody attached in time: its file is
// what the collector is about to delete.
func (s *Service) attachmentInUse(ctx context.Context, q store.DB, key string) (bool, error) {
	id, err := uuid.Parse(strings.TrimSuffix(path.Base(key), "."+blob.Ext(key)))
	if err != nil {
		return false, nil //nolint:nilerr // not a key this module made: nothing refers to it
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM attachments WHERE id = $1 AND message_id IS NULL AND created_at <= now() - $2::interval`,
		id, interval(attachWindow)); err != nil {
		return false, err
	}
	var used bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attachments WHERE id = $1)`, id).Scan(&used)
	return used, err
}

// interval is d as a Postgres interval.
func interval(d time.Duration) string { return strconv.Itoa(int(d/time.Second)) + " seconds" }
