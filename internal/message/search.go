package message

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Search in messages (docs/adr/0039-search.md).
const (
	searchPage      = 25
	maxSearchOffset = 5000
	maxSearchQuery  = 200
)

// searchTimeout bounds one search in the database.
const searchTimeout = 3 * time.Second

var (
	errSearchTooSlow = httpx.NewError(http.StatusUnprocessableEntity, "search_too_slow",
		"Поиск занял слишком много времени: добавьте слова или выберите канал")
	ruleSearch       = ratelimit.Rule{Name: "search", Limit: 1, Window: time.Second}
	ruleSearchMinute = ratelimit.Rule{Name: "search_minute", Limit: 30, Window: time.Minute}
	errEmptySearch   = httpx.Validation(map[string]string{"q": "Нужен текст или хотя бы один фильтр"})
)

// Has filters by what a message carries.
type Has string

const (
	HasFile  Has = "file"
	HasImage Has = "image"
	HasVideo Has = "video"
	HasLink  Has = "link"
)

// SearchQuery is what to look for; every set field narrows the results.
type SearchQuery struct {
	Text      string
	AuthorID  *uuid.UUID
	ChannelID *uuid.UUID
	Mentions  *uuid.UUID
	Has       []Has
	Before    *time.Time
	After     *time.Time
	Offset    int
}

// SearchResult is a page of matches, newest first. Total counts at most
// maxSearchOffset+searchPage matches: "more than five thousand" is enough.
type SearchResult struct {
	Total    int       `json:"total_results"`
	Messages []Message `json:"messages"`
}

func (q SearchQuery) validate() error {
	fields := httpx.Fields{}
	if utf8.RuneCountInString(q.Text) > maxSearchQuery || strings.ContainsRune(q.Text, 0) {
		fields["q"] = "До " + strconv.Itoa(maxSearchQuery) + " символов"
	}
	if q.Offset < 0 || q.Offset > maxSearchOffset {
		fields["offset"] = "От 0 до " + strconv.Itoa(maxSearchOffset)
	}
	for _, h := range q.Has {
		if h != HasFile && h != HasImage && h != HasVideo && h != HasLink {
			fields["has"] = "file, image, video или link"
		}
	}
	if err := fields.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(q.Text) == "" && q.AuthorID == nil && q.Mentions == nil && len(q.Has) == 0 {
		return errEmptySearch
	}
	return nil
}

// SearchGuild searches the text channels of a guild the user may read; a
// channel filter outside them finds nothing, as if it did not exist.
func (s *Service) SearchGuild(ctx context.Context, userID, guildID uuid.UUID, q SearchQuery) (SearchResult, error) {
	if err := q.validate(); err != nil {
		return SearchResult{}, err
	}
	channels, _, err := s.guilds.Readable(ctx, userID, &guildID)
	if err != nil {
		return SearchResult{}, err
	}
	if q.ChannelID != nil {
		if !slices.Contains(channels, *q.ChannelID) {
			return SearchResult{Messages: []Message{}}, nil
		}
		channels = []uuid.UUID{*q.ChannelID}
	}
	return s.search(ctx, userID, channels, q)
}

// SearchChannel searches one channel: a DM, a group or a guild channel.
func (s *Service) SearchChannel(ctx context.Context, userID, channelID uuid.UUID, q SearchQuery) (SearchResult, error) {
	if err := q.validate(); err != nil {
		return SearchResult{}, err
	}
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return SearchResult{}, err
	}
	if !ch.perms.Has(perm.ReadMessageHistory) {
		return SearchResult{}, httpx.ErrForbidden
	}
	return s.search(ctx, userID, []uuid.UUID{channelID}, q)
}

// hasFilters are the SQL conditions of has:… filters.
var hasFilters = map[Has]string{
	HasFile:  `EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`,
	HasImage: `EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.ext IN ('png', 'jpg', 'gif', 'webp'))`,
	HasVideo: `EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.ext IN ('mp4', 'webm'))`,
	HasLink:  `m.content ~ 'https?://'`,
}

func (s *Service) search(ctx context.Context, userID uuid.UUID, channels []uuid.UUID, q SearchQuery) (SearchResult, error) {
	out := SearchResult{Messages: []Message{}}
	if len(channels) == 0 {
		return out, nil
	}
	if err := s.limiter.Check(ctx, ruleSearch, userID.String()); err != nil {
		return out, err
	}
	if err := s.limiter.Check(ctx, ruleSearchMinute, userID.String()); err != nil {
		return out, err
	}
	where := []string{`m.channel_id = ANY($1)`}
	args := []any{channels}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if text := strings.TrimSpace(q.Text); text != "" {
		p := arg(text)
		where = append(where, `m.search @@ (websearch_to_tsquery('russian', `+p+`) || websearch_to_tsquery('simple', `+p+`))`)
	}
	if q.AuthorID != nil {
		where = append(where, `m.author_id = `+arg(*q.AuthorID))
	}
	if q.Mentions != nil {
		// @> uses the GIN index on mentions; = ANY does not.
		where = append(where, `m.mentions @> ARRAY[`+arg(*q.Mentions)+`]::uuid[]`)
	}
	for _, h := range q.Has {
		where = append(where, hasFilters[h])
	}
	if q.Before != nil {
		where = append(where, `m.created_at < `+arg(*q.Before))
	}
	if q.After != nil {
		where = append(where, `m.created_at > `+arg(*q.After))
	}
	cond := strings.Join(where, " AND ")

	offset := arg(q.Offset)
	var ms []Message
	// A search without text can walk every message of a server: it gets a
	// few seconds, not the database (S5 of docs/audit/2026-11-04.md).
	err := pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = `+strconv.Itoa(int(searchTimeout/time.Millisecond))); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM (SELECT 1 FROM messages m WHERE `+cond+` LIMIT `+strconv.Itoa(maxSearchOffset+searchPage)+`) x`,
			args[:len(args)-1]...).Scan(&out.Total); err != nil {
			return err
		}
		if out.Total == 0 || q.Offset >= out.Total {
			return nil
		}
		// The select list is unqualified: only m is in scope there.
		rows, err := tx.Query(ctx,
			`SELECT `+msgCols+` FROM messages m WHERE `+cond+` ORDER BY m.id DESC LIMIT `+strconv.Itoa(searchPage)+` OFFSET `+offset,
			args...)
		if err != nil {
			return err
		}
		ms, err = pgx.CollectRows(rows, scanMessage)
		return err
	})
	if isTimeout(err) {
		return out, errSearchTooSlow
	}
	if err != nil || len(ms) == 0 {
		return out, err
	}
	if err := s.fill(ctx, s.pg, ms); err != nil {
		return out, err
	}
	if err := withReactions(ctx, s.pg, ms, userID); err != nil {
		return out, err
	}
	out.Messages = ms
	return out, nil
}

// isTimeout reports a statement cancelled by statement_timeout.
func isTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// parseSearch reads a search from the query string.
func parseSearch(r *http.Request) (SearchQuery, error) {
	v := r.URL.Query()
	q := SearchQuery{Text: v.Get("q")}
	var err error
	for _, f := range []struct {
		key string
		dst **uuid.UUID
	}{{"author_id", &q.AuthorID}, {"channel_id", &q.ChannelID}, {"mentions", &q.Mentions}} {
		if *f.dst, err = httpx.QueryUUID(v, f.key, "id"); err != nil {
			return q, err
		}
	}
	for _, h := range v["has"] {
		q.Has = append(q.Has, Has(h))
	}
	for _, f := range []struct {
		key string
		dst **time.Time
	}{{"before", &q.Before}, {"after", &q.After}} {
		if s := v.Get(f.key); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return q, httpx.Validation(map[string]string{f.key: "Дата и время в RFC 3339, например 2026-11-04T00:00:00Z"})
			}
			*f.dst = &t
		}
	}
	if s := v.Get("offset"); s != "" {
		if q.Offset, err = strconv.Atoi(s); err != nil {
			return q, httpx.Validation(map[string]string{"offset": "Число"})
		}
	}
	return q, nil
}
