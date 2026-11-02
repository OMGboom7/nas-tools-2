package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// EmitInteractiveReply freezes one authenticated incoming channel and sender.
// Command acknowledgements do not broadcast to every notification recipient.
func EmitInteractiveReply(ctx context.Context, tx *sql.Tx, key, reference, source, user string, channelID int64) error {
	if channelID <= 0 || len(user) == 0 || len(user) > 512 {
		return ErrState
	}
	c, e := scanChannel(tx.QueryRowContext(ctx, channelsSQL+` WHERE ID=?`, channelID))
	if e != nil || Hash(c) != source || !selected(c, "site_signin_ack") {
		return ErrState
	}
	if e = schema(ctx, tx); e != nil {
		return e
	}
	id := Hash([]string{"site_signin_ack", key})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, e := tx.ExecContext(ctx, `INSERT INTO GO_BUSINESS_EVENTS VALUES(?,?,?,?,?,?) ON CONFLICT(ID) DO NOTHING`, id, "site_signin_ack", reference, "已收到站点签到请求", "已保存本次 /pts；签到结果按原记录核对", now)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil || n == 0 {
		return e
	}
	data, _ := json.Marshal(map[string]string{"user_id": user})
	if _, e = tx.ExecContext(ctx, `INSERT INTO GO_BUSINESS_EVENT_DATA VALUES(?,?)`, id, string(data)); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO GO_NOTIFICATION_DELIVERIES(ID,EVENT_ID,CHANNEL_ID,CHANNEL_NAME,CHANNEL_TYPE,CONFIG_DIGEST,STATE,UPDATED) VALUES(?,?,?,?,?,?,'pending',?)`, Hash([]string{id, fmt.Sprint(c.ID)}), id, c.ID, c.Name, c.Type, Hash(c), now)
	return e
}
