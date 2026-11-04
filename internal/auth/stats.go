package auth

import (
	"context"

	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

// keepDays is how long user_days rows live: the longest report.
const keepDays = 90

// Stats counts accounts, sign-ups by link tag and active users for owner
// reports (docs/adr/0042-owner-stats.md).
func (s *Service) Stats(ctx context.Context, q stats.Query) (stats.Part, error) {
	pg := s.store.pg
	today := stats.Day(s.now())
	// Reports read activity, so they also drop what no report shows.
	if _, err := pg.Exec(ctx, `DELETE FROM user_days WHERE day < $1::date - $2::int`, today, keepDays); err != nil {
		return stats.Part{}, err
	}
	from := stats.FirstID(q.From)
	c := stats.NewCounter(ctx, pg)
	c.Total("users", `SELECT count(*) FROM users WHERE deleted_at IS NULL`)
	c.Now("active_day", `SELECT count(*) FROM user_days WHERE day = $1::date`, today)
	c.Now("active_week", `SELECT count(DISTINCT user_id) FROM user_days WHERE day > $1::date - 7`, today)
	c.Now("active_month", `SELECT count(DISTINCT user_id) FROM user_days WHERE day > $1::date - 30`, today)
	c.Days("registrations", `SELECT `+stats.DayOf("created_at")+`::text, count(*) FROM users WHERE id >= $1 GROUP BY 1`, from)
	c.Days("active_users", `SELECT day::text, count(*) FROM user_days WHERE day >= $1::date GROUP BY day`, stats.Day(q.From))
	c.Refs("registrations", `SELECT ref, count(*) FROM users WHERE id >= $1 GROUP BY ref`, from)
	return c.Part()
}

// markActive records that the user opened the app today. A lost mark only
// lowers a count by one, so failures are not the caller's problem.
func (s *Service) markActive(ctx context.Context, p Principal) {
	_, _ = s.store.pg.Exec(ctx, `INSERT INTO user_days (day, user_id) VALUES ($1::date, $2) ON CONFLICT DO NOTHING`,
		stats.Day(s.now()), p.UserID)
}
