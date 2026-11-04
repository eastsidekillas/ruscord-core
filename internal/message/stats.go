package message

import (
	"context"

	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

// Stats counts messages, people who wrote and files for owner reports
// (docs/adr/0042-owner-stats.md). Ids are UUIDv7, so the period is a range
// of the primary key.
func (s *Service) Stats(ctx context.Context, q stats.Query) (stats.Part, error) {
	from := stats.FirstID(q.From)
	day := stats.DayOf("created_at")
	c := stats.NewCounter(ctx, s.pg)
	c.Total("messages", `SELECT count(*) FROM messages`)
	c.Total("attachment_bytes", `SELECT coalesce(sum(size), 0)::bigint FROM attachments WHERE message_id IS NOT NULL`)
	c.Days("messages", `SELECT `+day+`::text, count(*) FROM messages WHERE id >= $1 GROUP BY 1`, from)
	c.Days("senders", `SELECT d::text, count(DISTINCT author_id) FROM (SELECT `+day+` AS d, author_id FROM messages WHERE id >= $1) m GROUP BY d`, from)
	c.Days("attachments", `SELECT `+day+`::text, count(*) FROM attachments WHERE id >= $1 AND message_id IS NOT NULL GROUP BY 1`, from)
	return c.Part()
}
