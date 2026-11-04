// Package hooks defines callbacks that let one module react to another's
// changes inside the same transaction, without importing it.
package hooks

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChannelsDeleted runs in the transaction that deletes the channels, so data
// keyed by channel id (messages) goes away atomically with them. The caller
// has locked the channel rows with LockForDelete.
type ChannelsDeleted func(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error

// Run calls h if it is set.
func (h ChannelsDeleted) Run(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error {
	if h == nil || len(channelIDs) == 0 {
		return nil
	}
	return h(ctx, tx, channelIDs)
}

// Writes that other modules key by channel id (messages, uploads, read
// marks) race with the deletion of the channel: checked before their
// transaction, they could commit after the hook has run and stay for ever.
// So the owner of the channels locks their rows FOR UPDATE before it runs
// ChannelsDeleted, and such a write locks the channel first with
// LockChannels: either it commits first and the hook deletes it, or it
// waits and finds the channel gone.

// LockChannels locks the rows of table (the owner's channel table) with the
// given ids FOR KEY SHARE until tx ends and returns the ids that exist. KEY
// SHARE stops only deletion: edits of the channel still go through.
func LockChannels(ctx context.Context, tx pgx.Tx, table string, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM `+table+` WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// LockForDelete locks the channel rows FOR UPDATE before ChannelsDeleted
// runs; it waits for writes that hold them with LockChannels.
func LockForDelete(ctx context.Context, tx pgx.Tx, table string, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM `+table+` WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	return err
}
