package presence

import (
	"context"
	"strings"

	"github.com/eastsidekillas/ruscord-core/internal/stats"
)

// Stats counts who is online now for owner reports
// (docs/adr/0042-owner-stats.md): live sessions and their distinct users.
// It walks the session keys, so reports are cached by the stats module.
func (s *Service) Stats(ctx context.Context, _ stats.Query) (stats.Part, error) {
	users := map[string]bool{}
	sessions := 0
	iter := s.rdb.Scan(ctx, 0, "presence:sess:*", 1000).Iterator()
	for iter.Next(ctx) {
		// presence:sess:<user>:<session>
		rest := strings.TrimPrefix(iter.Val(), "presence:sess:")
		if user, _, ok := strings.Cut(rest, ":"); ok {
			users[user] = true
			sessions++
		}
	}
	if err := iter.Err(); err != nil {
		return stats.Part{}, err
	}
	return stats.Part{Now: map[string]int{"online": len(users), "sessions": sessions}}, nil
}
