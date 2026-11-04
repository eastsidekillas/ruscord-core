// Package stats gives the owners of the service counts of how it is used
// (docs/adr/0042-owner-stats.md): sign-ups, active users, messages, who is
// online, where people came from. Counts only: no list of people, nothing
// they wrote. Each module counts its own tables (Source); this package
// puts the parts together and keeps its one table, site visits.
package stats

import (
	"context"
	"encoding/binary"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Zone is where days begin: the audience lives mostly in Moscow time.
const Zone = "Europe/Moscow"

var zone = func() *time.Location {
	l, err := time.LoadLocation(Zone)
	if err != nil {
		return time.FixedZone("MSK", 3*60*60)
	}
	return l
}()

// Day is the Moscow date of t as Postgres prints a date.
func Day(t time.Time) string { return t.In(zone).Format(time.DateOnly) }

// DayOf is an SQL expression for the Moscow date of a timestamptz column.
func DayOf(col string) string { return `(` + col + ` AT TIME ZONE '` + Zone + `')::date` }

// FirstID is the smallest UUIDv7 made at t or later. Ids are UUIDv7 in every
// table, so "id >= FirstID(t)" finds rows made since t by the primary key.
func FirstID(t time.Time) uuid.UUID {
	var u uuid.UUID
	ms := uint64(max(t.UnixMilli(), 0)) //nolint:gosec // clamped at zero
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ms<<16)
	copy(u[:6], b[:6])
	u[6] = 0x70 // version 7
	u[8] = 0x80 // RFC 4122 variant
	return u
}

// refRe is what a link tag may be; anything else counts as no tag.
var refRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Ref is the tag of ?ref= as stored: lower case, or "" when it is none.
func Ref(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !refRe.MatchString(s) {
		return ""
	}
	return s
}

// Query is the period a report covers: whole Moscow days from From.
type Query struct {
	From time.Time
}

// Part is what one module counts. Every map is keyed by metric name.
type Part struct {
	// Now is the state at this moment (online, in voice).
	Now map[string]int
	// Totals are counts over all time (accounts, messages).
	Totals map[string]int
	// Days are counts per day of the period, keyed by Day.
	Days map[string]map[string]int
	// Refs are counts per link tag over the period ("" for none).
	Refs map[string]map[string]int
}

// Source is a module that counts its own tables.
type Source interface {
	Stats(ctx context.Context, q Query) (Part, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context, q Query) (Part, error)

func (f SourceFunc) Stats(ctx context.Context, q Query) (Part, error) { return f(ctx, q) }

// Report is the answer of GET /admin/stats. Series hold one number per day
// of Days, oldest first.
type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Zone        string           `json:"zone"`
	Days        []string         `json:"days"`
	Now         map[string]int   `json:"now"`
	Totals      map[string]int   `json:"totals"`
	Series      map[string][]int `json:"series"`
	Refs        []RefRow         `json:"refs"`
}

// RefRow is what came through one link tag over the period.
type RefRow struct {
	Ref    string         `json:"ref"`
	Counts map[string]int `json:"counts"`
}

const (
	minDays, maxDays, defaultDays = 7, 90, 30
	// cacheFor keeps a report: owners reload the page, the counts scan days
	// of rows.
	cacheFor = 30 * time.Second
)

var (
	// ruleVisit counts a visitor once per half hour, by address; the
	// address stays only in the limiter key, which expires with it.
	ruleVisit = ratelimit.Rule{Name: "visit", Limit: 1, Window: 30 * time.Minute}
	// ruleVisitDay caps visits counted per day: a flood of addresses must
	// not grow the table or the numbers without bound.
	ruleVisitDay = ratelimit.Rule{Name: "visit_day", Limit: 200_000, Window: 24 * time.Hour}
	errDays      = httpx.Validation(map[string]string{"days": "От 7 до 90"})
)

type cached struct {
	at     time.Time
	report Report
}

type Service struct {
	pg      *pgxpool.Pool
	limiter *ratelimit.Limiter
	admins  map[uuid.UUID]bool
	sources []Source
	now     func() time.Time

	mu    sync.Mutex
	cache map[int]cached
}

// NewService serves reports to admins only; to everyone else the page does
// not exist.
func NewService(pg *pgxpool.Pool, limiter *ratelimit.Limiter, admins []uuid.UUID, sources ...Source) *Service {
	s := &Service{pg: pg, limiter: limiter, admins: map[uuid.UUID]bool{}, now: time.Now, cache: map[int]cached{}}
	for _, id := range admins {
		s.admins[id] = true
	}
	s.sources = append(sources, SourceFunc(s.visits))
	return s
}

// IsAdmin reports whether the user may see reports.
func (s *Service) IsAdmin(userID uuid.UUID) bool { return s.admins[userID] }

// Report counts the last days days, today included.
func (s *Service) Report(ctx context.Context, userID uuid.UUID, days int) (Report, error) {
	if !s.IsAdmin(userID) {
		return Report{}, httpx.ErrNotFound
	}
	if days == 0 {
		days = defaultDays
	}
	if days < minDays || days > maxDays {
		return Report{}, errDays
	}
	now := s.now()
	s.mu.Lock()
	c, ok := s.cache[days]
	s.mu.Unlock()
	if ok && now.Sub(c.at) < cacheFor {
		return c.report, nil
	}
	r, err := s.build(ctx, now, days)
	if err != nil {
		return Report{}, err
	}
	s.mu.Lock()
	s.cache[days] = cached{at: now, report: r}
	s.mu.Unlock()
	return r, nil
}

func (s *Service) build(ctx context.Context, now time.Time, days int) (Report, error) {
	local := now.In(zone)
	first := time.Date(local.Year(), local.Month(), local.Day()-days+1, 0, 0, 0, 0, zone)
	r := Report{
		GeneratedAt: now.UTC(), Zone: Zone,
		Now: map[string]int{}, Totals: map[string]int{}, Series: map[string][]int{}, Refs: []RefRow{},
	}
	for i := range days {
		r.Days = append(r.Days, first.AddDate(0, 0, i).Format(time.DateOnly))
	}
	refs := map[string]map[string]int{}
	for _, src := range s.sources {
		p, err := src.Stats(ctx, Query{From: first})
		if err != nil {
			return Report{}, err
		}
		maps.Copy(r.Now, p.Now)
		maps.Copy(r.Totals, p.Totals)
		for name, byDay := range p.Days {
			series := make([]int, days)
			for i, d := range r.Days {
				series[i] = byDay[d]
			}
			r.Series[name] = series
		}
		for name, byRef := range p.Refs {
			for ref, n := range byRef {
				if refs[ref] == nil {
					refs[ref] = map[string]int{}
				}
				refs[ref][name] += n
			}
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		r.Refs = append(r.Refs, RefRow{Ref: ref, Counts: refs[ref]})
	}
	return r, nil
}

// Visit counts a visit of the site through ref, once per address per half
// hour. Visits above the limits are dropped silently.
func (s *Service) Visit(ctx context.Context, ip, ref string) error {
	if ok, _, err := s.limiter.Allow(ctx, ruleVisit, ip); err != nil || !ok {
		return err
	}
	if ok, _, err := s.limiter.Allow(ctx, ruleVisitDay, "all"); err != nil || !ok {
		return err
	}
	_, err := s.pg.Exec(ctx,
		`INSERT INTO site_visits (day, ref, count) VALUES ($1::date, $2, 1)
		 ON CONFLICT (day, ref) DO UPDATE SET count = site_visits.count + 1`, Day(s.now()), Ref(ref))
	return err
}

// visits is the Source of this package's own table.
func (s *Service) visits(ctx context.Context, q Query) (Part, error) {
	c := NewCounter(ctx, s.pg)
	c.Days("visits", `SELECT day::text, sum(count)::int FROM site_visits WHERE day >= $1::date GROUP BY day`, Day(q.From))
	c.Refs("visits", `SELECT ref, sum(count)::int FROM site_visits WHERE day >= $1::date GROUP BY ref`, Day(q.From))
	return c.Part()
}

// Counter fills a Part from SQL; modules write their Source with it. The
// first error stops the rest and is returned by Part.
type Counter struct {
	ctx  context.Context
	pg   *pgxpool.Pool
	part Part
	err  error
}

func NewCounter(ctx context.Context, pg *pgxpool.Pool) *Counter {
	return &Counter{ctx: ctx, pg: pg, part: Part{
		Now: map[string]int{}, Totals: map[string]int{}, Days: map[string]map[string]int{}, Refs: map[string]map[string]int{},
	}}
}

// Now sets a number of this moment; sql selects one number.
func (c *Counter) Now(name, sql string, args ...any) { c.one(c.part.Now, name, sql, args) }

// Total sets an all-time number; sql selects one number.
func (c *Counter) Total(name, sql string, args ...any) { c.one(c.part.Totals, name, sql, args) }

// Days sets a series; sql selects (day::text, count) rows.
func (c *Counter) Days(name, sql string, args ...any) { c.many(c.part.Days, name, sql, args) }

// Refs sets counts per link tag; sql selects (ref, count), NULL for none.
func (c *Counter) Refs(name, sql string, args ...any) { c.many(c.part.Refs, name, sql, args) }

// Part returns what was counted.
func (c *Counter) Part() (Part, error) { return c.part, c.err }

func (c *Counter) one(dst map[string]int, name, sql string, args []any) {
	if c.err != nil {
		return
	}
	var n int
	c.err = c.pg.QueryRow(c.ctx, sql, args...).Scan(&n)
	dst[name] = n
}

func (c *Counter) many(dst map[string]map[string]int, name, sql string, args []any) {
	if c.err != nil {
		return
	}
	out := map[string]int{}
	rows, err := c.pg.Query(c.ctx, sql, args...)
	if err != nil {
		c.err = err
		return
	}
	var key *string
	var n int
	_, c.err = pgx.ForEachRow(rows, []any{&key, &n}, func() error {
		k := ""
		if key != nil {
			k = *key
		}
		out[k] += n
		return nil
	})
	dst[name] = out
}
