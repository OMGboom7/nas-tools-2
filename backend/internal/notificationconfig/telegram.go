package notificationconfig

import (
	"context"
	"database/sql"
	"errors"
)

const telegramHandoffSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_HANDOFF
	(BOT TEXT NOT NULL, UPDATE_ID INTEGER NOT NULL, PRIMARY KEY(BOT,UPDATE_ID)) WITHOUT ROWID`

const currentTelegramChannel = `EXISTS(SELECT 1 FROM MESSAGE_CLIENT
		WHERE ID=? AND TYPE='telegram' AND CONFIG=? AND INTERACTIVE<>0 AND ENABLED<>0)
		AND (SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE TYPE='telegram' AND INTERACTIVE<>0 AND ENABLED<>0)=1`

// ClaimTelegramUpdate records the handoff before the legacy dispatcher is
// allowed to run an action. Multiple workers can receive the same callback.
func (s *Store) ClaimTelegramUpdate(ctx context.Context, channel Channel, bot string, update int64) (bool, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer lock before taking the channel snapshot.
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_HANDOFF(BOT,UPDATE_ID)
		SELECT ?,? WHERE `+currentTelegramChannel+` AND NOT EXISTS(SELECT 1 FROM GO_TELEGRAM_NATIVE_INPUT WHERE BOT=? AND UPDATE_ID=?)
		ON CONFLICT DO NOTHING`, bot, update, channel.ID, channel.Config, bot, update)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var active bool
		if err = tx.QueryRowContext(ctx, "SELECT "+currentTelegramChannel, channel.ID, channel.Config).Scan(&active); err != nil {
			return false, err
		}
		if !active {
			return false, errors.New("Telegram receiver changed")
		}
	}
	return n == 1, tx.Commit()
}

// TelegramReceiver selects exactly one enabled interactive bot.
func (s *Store) TelegramReceiver(ctx context.Context) (Channel, error) {
	return scanChannel(s.database.QueryRowContext(ctx, channelSelect+` WHERE TYPE='telegram' AND INTERACTIVE<>0 AND ENABLED<>0
		AND (SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE TYPE='telegram' AND INTERACTIVE<>0 AND ENABLED<>0)=1`))
}

const telegramCursorSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_CURSOR
	(BOT TEXT PRIMARY KEY, NEXT_ID INTEGER NOT NULL, UPDATED INTEGER NOT NULL)`
const telegramModeSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_MODE_INTENT
	(BOT TEXT PRIMARY KEY, TARGET TEXT NOT NULL)`

func (s *Store) TelegramOffset(ctx context.Context, bot string, initial int64) (int64, error) {
	var next int64
	err := s.database.QueryRowContext(ctx, `SELECT CASE WHEN UPDATED>=unixepoch()-86400 THEN NEXT_ID ELSE 0 END
		FROM GO_TELEGRAM_CURSOR WHERE BOT=?`, bot).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return initial, nil
	}
	return next, err
}
func (s *Store) SaveTelegramOffset(ctx context.Context, bot string, next int64) error {
	_, err := s.database.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_CURSOR VALUES(?,?,unixepoch())
		ON CONFLICT(BOT) DO UPDATE SET NEXT_ID=excluded.NEXT_ID,UPDATED=excluded.UPDATED`, bot, next)
	return err
}

// BeginTelegramMode leaves an unresolved request on disk across a lost
// response or restart. A later observation of the desired mode clears it.
func (s *Store) BeginTelegramMode(ctx context.Context, bot, target string) (bool, error) {
	r, err := s.database.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_MODE_INTENT VALUES(?,?)
		ON CONFLICT(BOT) DO UPDATE SET TARGET=excluded.TARGET WHERE TARGET<>excluded.TARGET`, bot, target)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) ClearTelegramMode(ctx context.Context, bot string) error {
	_, err := s.database.ExecContext(ctx, "DELETE FROM GO_TELEGRAM_MODE_INTENT WHERE BOT=?", bot)
	return err
}

const telegramCallbackSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_CALLBACK
	(BOT TEXT NOT NULL, UPDATE_ID INTEGER NOT NULL, CHANNEL_ID INTEGER NOT NULL, CONFIG_HASH TEXT NOT NULL,
	MESSAGE BLOB NOT NULL, CREATED INTEGER NOT NULL DEFAULT (unixepoch()), PRIMARY KEY(BOT,UPDATE_ID))`

func (s *Store) QueueTelegram(ctx context.Context, ch Channel, bot string, id int64, configHash string, message []byte) error {
	res, err := s.database.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_CALLBACK(BOT,UPDATE_ID,CHANNEL_ID,CONFIG_HASH,MESSAGE)
		SELECT ?,?,?,?,? WHERE `+currentTelegramChannel+` AND NOT EXISTS(SELECT 1 FROM GO_TELEGRAM_HANDOFF WHERE BOT=? AND UPDATE_ID=?)
		AND NOT EXISTS(SELECT 1 FROM GO_TELEGRAM_NATIVE_INPUT WHERE BOT=? AND UPDATE_ID=?) ON CONFLICT DO NOTHING`,
		bot, id, ch.ID, configHash, message, ch.ID, ch.Config, bot, id, bot, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	current, err := s.TelegramReceiver(ctx)
	if err != nil {
		return err
	}
	if current.ID != ch.ID || current.Config != ch.Config {
		return errors.New("Telegram receiver changed")
	}
	return nil
}
func (s *Store) NextTelegramCallback(ctx context.Context, bot string, channel int64) (int64, string, []byte, error) {
	var id int64
	var hash string
	var raw []byte
	err := s.database.QueryRowContext(ctx, `SELECT UPDATE_ID,CONFIG_HASH,MESSAGE FROM GO_TELEGRAM_CALLBACK
		WHERE BOT=? AND CHANNEL_ID=? AND CREATED>=unixepoch()-86400 ORDER BY rowid LIMIT 1`, bot, channel).Scan(&id, &hash, &raw)
	return id, hash, raw, err
}
func (s *Store) RemoveTelegramCallback(ctx context.Context, bot string, id int64) error {
	_, err := s.database.ExecContext(ctx, "DELETE FROM GO_TELEGRAM_CALLBACK WHERE BOT=? AND UPDATE_ID=?", bot, id)
	return err
}

const telegramNativeInputSchema = `CREATE TABLE IF NOT EXISTS GO_TELEGRAM_NATIVE_INPUT
	(BOT TEXT NOT NULL, UPDATE_ID INTEGER NOT NULL, SOURCE TEXT NOT NULL, PRIMARY KEY(BOT,UPDATE_ID)) WITHOUT ROWID`

// The original native command journals remain responsible for their own
// retries. A different configuration or legacy consumer cannot reuse the ID.
func (s *Store) OwnNativeTelegram(ctx context.Context, ch Channel, bot string, update int64, source string) (bool, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_TELEGRAM_NATIVE_INPUT
		SELECT ?,?,? WHERE `+currentTelegramChannel+` AND NOT EXISTS(SELECT 1 FROM GO_TELEGRAM_HANDOFF WHERE BOT=? AND UPDATE_ID=?)
		ON CONFLICT DO NOTHING`, bot, update, source, ch.ID, ch.Config, bot, update)
	if err != nil {
		return false, err
	}
	var owned bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_TELEGRAM_NATIVE_INPUT WHERE BOT=? AND UPDATE_ID=? AND SOURCE=?) AND `+currentTelegramChannel,
		bot, update, source, ch.ID, ch.Config).Scan(&owned); err != nil {
		return false, err
	}
	return owned, tx.Commit()
}

// Telegram callbacks are transient transport data. Abandoned channels must
// not retain queued message bodies or execute them on a much later reinstall.
func (s *Store) PruneTelegramCallbacks(ctx context.Context) error {
	_, err := s.database.ExecContext(ctx, "DELETE FROM GO_TELEGRAM_CALLBACK WHERE CREATED<unixepoch()-86400")
	return err
}
