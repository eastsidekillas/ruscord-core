package guild

import (
	"context"

	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

// Stats counts servers, joins and people in voice for owner reports
// (docs/adr/0042-owner-stats.md).
func (s *Service) Stats(ctx context.Context, q stats.Query) (stats.Part, error) {
	c := stats.NewCounter(ctx, s.pg)
	c.Total("guilds", `SELECT count(*) FROM guilds`)
	c.Now("in_voice", `SELECT count(*) FROM voice_states`)
	c.Days("guilds", `SELECT `+stats.DayOf("created_at")+`::text, count(*) FROM guilds WHERE id >= $1 GROUP BY 1`, stats.FirstID(q.From))
	c.Days("joins", `SELECT `+stats.DayOf("joined_at")+`::text, count(*) FROM members WHERE joined_at >= $1 GROUP BY 1`, q.From)
	return c.Part()
}
