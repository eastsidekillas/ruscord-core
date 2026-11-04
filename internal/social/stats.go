package social

import (
	"context"

	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

// Stats counts people in DM calls for owner reports (docs/adr/0042-owner-stats.md).
func (s *Service) Stats(ctx context.Context, _ stats.Query) (stats.Part, error) {
	c := stats.NewCounter(ctx, s.pg)
	c.Now("in_calls", `SELECT count(*) FROM dm_call_members`)
	return c.Part()
}
