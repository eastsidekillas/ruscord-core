// Package hooks defines callbacks that let one module react to another's
// changes inside the same transaction, without importing it.
package hooks

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChannelsDeleted runs in the transaction that deletes the channels, so data
// keyed by channel id (messages) goes away atomically with them.
type ChannelsDeleted func(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error

// Run calls h if it is set.
func (h ChannelsDeleted) Run(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error {
	if h == nil || len(channelIDs) == 0 {
		return nil
	}
	return h(ctx, tx, channelIDs)
}
