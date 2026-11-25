package notificationconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const chatChoiceSchema = `CREATE TABLE IF NOT EXISTS GO_CHAT_CHOICE (
	CHANNEL TEXT NOT NULL, SENDER TEXT NOT NULL, REVISION TEXT NOT NULL,
	ACTION TEXT NOT NULL, CHOICES BLOB, PRIMARY KEY(CHANNEL,SENDER))`

type ChatChoiceReply struct {
	Action   string          `json:"action"`
	Query    string          `json:"query,omitempty"`
	Revision string          `json:"revision,omitempty"`
	Selected json.RawMessage `json:"selected,omitempty"`
}

// StartChatChoice replaces the previous menu before looking up a new title.
// Each lookup may publish only the revision it started, including across processes.
func (store *Store) StartChatChoice(ctx context.Context, channel, sender, message string, chatEnabled bool) (ChatChoiceReply, error) {
	msg := strings.TrimSpace(message)
	if channel == "" || sender == "" || msg == "" {
		return ChatChoiceReply{Action: "ignore"}, nil
	}
	if n, err := strconv.ParseUint(msg, 10, 64); err == nil && n < 10 {
		var mode string
		var data []byte
		err = store.database.QueryRowContext(ctx, "SELECT ACTION,CHOICES FROM GO_CHAT_CHOICE WHERE CHANNEL=? AND SENDER=?", channel, sender).Scan(&mode, &data)
		if errors.Is(err, sql.ErrNoRows) {
			return ChatChoiceReply{Action: "invalid"}, nil
		}
		if err != nil {
			return ChatChoiceReply{}, err
		}
		var rows []json.RawMessage
		if len(data) > 0 {
			if err := json.Unmarshal(data, &rows); err != nil {
				return ChatChoiceReply{}, err
			}
		}
		if n == 0 || n > uint64(len(rows)) {
			return ChatChoiceReply{Action: "invalid"}, nil
		}
		return ChatChoiceReply{Action: mode, Selected: rows[n-1]}, nil
	}
	action, query := "search", msg
	switch {
	case strings.HasPrefix(msg, "订阅"):
		action, query = "subscribe", strings.TrimPrefix(msg, "订阅")
	case strings.HasPrefix(msg, "http"):
		action = "download"
	case strings.HasPrefix(msg, "搜索"):
		query = strings.TrimPrefix(msg, "搜索")
	case strings.HasPrefix(msg, "下载"):
		query = strings.TrimPrefix(msg, "下载")
	case chatEnabled:
		action = "ask"
	}
	if action == "search" || action == "subscribe" {
		if query != msg {
			query = strings.TrimLeft(strings.TrimSpace(query), ":：")
			query = strings.TrimSpace(query)
		}
	} else {
		_, err := store.database.ExecContext(ctx, "DELETE FROM GO_CHAT_CHOICE WHERE CHANNEL=? AND SENDER=?", channel, sender)
		return ChatChoiceReply{Action: action, Query: query}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ChatChoiceReply{}, err
	}
	revision := hex.EncodeToString(nonce[:])
	_, err := store.database.ExecContext(ctx, `INSERT INTO GO_CHAT_CHOICE (CHANNEL,SENDER,REVISION,ACTION) VALUES(?,?,?,?)
	ON CONFLICT(CHANNEL,SENDER) DO UPDATE SET REVISION=excluded.REVISION,ACTION=excluded.ACTION,CHOICES=NULL`, channel, sender, revision, action)
	return ChatChoiceReply{Action: action, Query: query, Revision: revision}, err
}

func (store *Store) PublishChatChoices(ctx context.Context, channel, sender, revision string, rows []json.RawMessage) (ChatChoiceReply, error) {
	if revision == "" || len(rows) == 0 || len(rows) > 8 {
		return ChatChoiceReply{}, errors.New("invalid title choices")
	}
	for _, row := range rows {
		var candidate struct {
			Title string
			Kind  string
		}
		if json.Unmarshal(row, &candidate) != nil || candidate.Title == "" || (candidate.Kind != "电影" && candidate.Kind != "电视剧" && candidate.Kind != "动漫") {
			return ChatChoiceReply{}, errors.New("invalid title choice")
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return ChatChoiceReply{}, err
	}
	var action string
	err = store.database.QueryRowContext(ctx, `UPDATE GO_CHAT_CHOICE SET CHOICES=? WHERE CHANNEL=? AND SENDER=? AND REVISION=? AND CHOICES IS NULL RETURNING ACTION`, data, channel, sender, revision).Scan(&action)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatChoiceReply{Action: "superseded"}, nil
	}
	if err != nil {
		return ChatChoiceReply{}, err
	}
	if len(rows) == 1 {
		return ChatChoiceReply{Action: action, Selected: rows[0]}, nil
	}
	return ChatChoiceReply{Action: "menu"}, nil
}
