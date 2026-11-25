package notificationconfig

import (
	"context"
	"errors"
)

const telegramHandoffSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_HANDOFF
	(BOT TEXT NOT NULL, UPDATE_ID INTEGER NOT NULL, PRIMARY KEY(BOT,UPDATE_ID)) WITHOUT ROWID`

// ClaimTelegramUpdate records the handoff before the legacy dispatcher is
// allowed to run an action. Multiple workers can receive the same callback.
func (s *Store) ClaimTelegramUpdate(ctx context.Context, channel Channel, bot string, update int64) (bool, error) {
	const current = `EXISTS(SELECT 1 FROM MESSAGE_CLIENT
		WHERE ID=? AND TYPE='telegram' AND CONFIG=? AND INTERACTIVE<>0 AND ENABLED<>0)
		AND (SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE TYPE='telegram' AND INTERACTIVE<>0 AND ENABLED<>0)=1`
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer lock before taking the channel snapshot.
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_HANDOFF(BOT,UPDATE_ID)
		SELECT ?,? WHERE `+current+` ON CONFLICT DO NOTHING`, bot, update, channel.ID, channel.Config)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var active bool
		if err = tx.QueryRowContext(ctx, "SELECT "+current, channel.ID, channel.Config).Scan(&active); err != nil {
			return false, err
		}
		if !active {
			return false, errors.New("Telegram receiver changed")
		}
	}
	return n == 1, tx.Commit()
}
