package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

type telegramDispatch struct {
	Action string `json:"action"`
	UserID int64  `json:"user_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Text   string `json:"text,omitempty"`
}

func decodeTelegramUpdate(raw []byte) (signinTelegramUpdate, bool) {
	var u signinTelegramUpdate
	if json.Unmarshal(raw, &u) != nil || u.Message.From.Bot || u.Message.Text == "" || len(u.Message.Text) > 32000 || len(u.Message.From.Name) > 1000 {
		return u, false
	}
	id, err := u.ID.Int64()
	user, userErr := u.Message.From.ID.Int64()
	return u, err == nil && id >= 0 && userErr == nil && user > 0
}

func telegramSenderAllowed(cfg map[string]any, user, body string) bool {
	admins := signinStringList(text(cfg["admin_ids"]), ",")
	return admins[user] || (!strings.HasPrefix(body, "/") && signinStringList(text(cfg["user_ids"]), ",")[user])
}

// TelegramIncomingCommand admits messages for a running legacy dispatcher.
// It takes peer metadata from the local caller, never from the Telegram body.
func TelegramIncomingCommand(ctx context.Context, path, channelID, peer string, in io.Reader, out io.Writer) error {
	id, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || id <= 0 {
		return errors.New("invalid Telegram channel")
	}
	raw, err := io.ReadAll(io.LimitReader(in, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return errors.New("invalid Telegram callback")
	}
	cfg, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(peer)
	encoder := json.NewEncoder(out)
	if err != nil || !signinIPAllowed(objectValue(cfg["security"])["telegram_webhook_allow_ip"], ip.Unmap()) {
		return encoder.Encode(telegramDispatch{Action: "forbidden"})
	}
	u, ok := decodeTelegramUpdate(raw)
	if !ok {
		if !json.Valid(raw) {
			return encoder.Encode(telegramDispatch{Action: "invalid"})
		}
		return encoder.Encode(telegramDispatch{Action: "ignore"})
	}
	store, err := notificationconfig.Open(filepath.Join(filepath.Dir(path), "user.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	ch, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	var channel map[string]any
	if ch.Type != "telegram" || ch.Interactive == 0 || ch.Enabled == 0 || json.Unmarshal([]byte(ch.Config), &channel) != nil {
		return errors.New("unavailable Telegram receiver")
	}
	token := text(channel["token"])
	if !validTelegramToken(token) {
		return errors.New("invalid Telegram receiver")
	}
	user, _ := u.Message.From.ID.Int64()
	result := telegramDispatch{Action: "dispatch", UserID: user, Name: u.Message.From.Name, Text: u.Message.Text}
	if !telegramSenderAllowed(channel, strconv.FormatInt(user, 10), result.Text) {
		result = telegramDispatch{Action: "deny", UserID: user, Text: "你不在用户白名单中，无法使用此机器人"}
		if strings.HasPrefix(u.Message.Text, "/") {
			result.Text = "只有管理员才有权限执行此命令"
		}
	}
	// Bot IDs survive token rotation. Nonstandard tokens on compatible
	// gateways use a digest and never enter the handoff table in cleartext.
	bot, _, hasID := strings.Cut(token, ":")
	if id, e := strconv.ParseInt(bot, 10, 64); !hasID || e != nil || id <= 0 {
		digest := sha256.Sum256([]byte(token))
		bot = hex.EncodeToString(digest[:])
	}
	updateID, _ := u.ID.Int64()
	claimed, err := store.ClaimTelegramUpdate(ctx, ch, bot, updateID)
	if err != nil {
		return err
	}
	if !claimed {
		result = telegramDispatch{Action: "ignore"}
	}
	return encoder.Encode(result)
}
