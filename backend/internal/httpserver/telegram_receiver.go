package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func holdTelegramReceiver(ctx context.Context, path string) (func(), error) {
	return holdVolumeReceiver(ctx, path, "telegram-receiver", ".nastool-telegram-receiver.lock")
}

type telegramReceiver struct {
	store    *notificationconfig.Store
	settings *config.Store
	out      *json.Encoder
}

type telegramSettings struct {
	source           siteSigninRemoteSource
	cfg              map[string]any
	bot, fingerprint string
	callbackSource   string
}

func (r telegramReceiver) snapshot(ctx context.Context) (telegramSettings, error) {
	var current telegramSettings
	ch, err := r.store.TelegramReceiver(ctx)
	if err != nil {
		return current, err
	}
	current.cfg, err = r.settings.Snapshot()
	if err != nil {
		return current, err
	}
	current.source = siteSigninRemoteSource{channel: ch, mode: "webhook"}
	if json.Unmarshal([]byte(ch.Config), &current.source.config) != nil || text(current.source.config["chat_id"]) == "" {
		return current, errors.New("invalid Telegram receiver")
	}
	if err = current.source.configureTelegram(current.cfg); err != nil {
		return current, err
	}
	current.bot = telegramBotID(text(current.source.config["token"]))
	current.fingerprint = sitelogin.Hash([]any{ch.ID, ch.Config, current.source.callback, objectValue(current.cfg["laboratory"])["telegram_domain"], objectValue(current.cfg["app"])["proxies"], objectValue(current.cfg["security"])["telegram_webhook_allow_ip"]})
	current.callbackSource = sitelogin.Hash([]any{ch.ID, current.bot, objectValue(current.cfg["laboratory"])["telegram_domain"], objectValue(current.cfg["security"])["api_key"]})
	return current, nil
}

func (r telegramReceiver) current(ctx context.Context, s telegramSettings) error {
	now, err := r.snapshot(ctx)
	if err != nil {
		return err
	}
	if now.fingerprint != s.fingerprint {
		return errors.New("Telegram settings changed")
	}
	return nil
}

type telegramBackoff struct{ wait time.Duration }

func (e telegramBackoff) Error() string { return "Telegram rate limit" }

var errTelegramRefused = errors.New("Telegram rejected request")

func (r telegramReceiver) call(ctx context.Context, s telegramSettings, method string, values url.Values, out any) error {
	if err := r.current(ctx, s); err != nil {
		return err
	}
	req, transport, err := telegramReceiveRequest(ctx, s.cfg, text(s.source.config["token"]), method, values, nil)
	if err != nil {
		return err
	}
	client := notificationService{client: &http.Client{Transport: transport}}.notificationHTTPClient()
	if transport != nil {
		defer client.CloseIdleConnections()
	}
	req.Close = true
	req.GetBody = nil
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Telegram response unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 || json.Unmarshal(raw, out) != nil {
		return errors.New("invalid Telegram response")
	}
	if resp.StatusCode != 200 {
		var receipt struct {
			OK         *bool
			Parameters struct {
				Retry int64 `json:"retry_after"`
			}
		}
		if resp.StatusCode == 429 && json.Unmarshal(raw, &receipt) == nil && receipt.OK != nil && !*receipt.OK && receipt.Parameters.Retry > 0 {
			return telegramBackoff{time.Duration(min(receipt.Parameters.Retry, 86400)) * time.Second}
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && json.Unmarshal(raw, &receipt) == nil && receipt.OK != nil && !*receipt.OK {
			return errTelegramRefused
		}
		return errors.New("Telegram response unavailable")
	}
	return r.current(ctx, s)
}

func (r telegramReceiver) reconcile(ctx context.Context, s telegramSettings) error {
	var info struct {
		OK     bool
		Result *struct{ URL *string }
	}
	if err := r.call(ctx, s, "getWebhookInfo", nil, &info); err != nil {
		return err
	}
	if !info.OK || info.Result == nil || info.Result.URL == nil {
		return errors.New("unable to verify Telegram mode")
	}
	if *info.Result.URL == s.source.callback {
		return r.store.ClearTelegramMode(ctx, s.bot)
	}
	method, values := telegramModeRequest(s.source.callback)
	claimed, err := r.store.BeginTelegramMode(ctx, s.bot, sitelogin.Hash([]any{s.source.config["token"], s.source.callback, objectValue(s.cfg["laboratory"])["telegram_domain"]}))
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("Telegram mode change awaiting confirmation")
	}
	var result struct{ OK, Result *bool }
	if err = r.call(ctx, s, method, values, &result); err != nil {
		var quota telegramBackoff
		if errors.Is(err, errTelegramRefused) || errors.As(err, &quota) {
			if e := r.store.ClearTelegramMode(ctx, s.bot); e != nil {
				return e
			}
		}
		return err
	}
	if result.OK != nil && !*result.OK {
		if err := r.store.ClearTelegramMode(ctx, s.bot); err != nil {
			return err
		}
		return errTelegramRefused
	}
	if result.OK == nil || result.Result == nil || !*result.Result {
		return errors.New("Telegram mode change not confirmed")
	}
	return r.store.ClearTelegramMode(ctx, s.bot)
}

func (r telegramReceiver) handoff(ctx context.Context, s telegramSettings, raw []byte) error {
	u, ok := decodeTelegramUpdate(raw)
	if !ok {
		return nil
	}
	return r.dispatch(ctx, s, u)
}
func (r telegramReceiver) dispatch(ctx context.Context, s telegramSettings, u signinTelegramUpdate) error {
	if err := r.current(ctx, s); err != nil {
		return err
	}
	result, err := admitTelegram(ctx, r.store, s.source.channel, u)
	if err != nil {
		return err
	}
	if result.Action == "dispatch" || result.Action == "deny" {
		return r.out.Encode(result)
	}
	return nil
}

type queuedTelegram struct {
	Message signinTelegramUpdate
	Peer    string
	Allowed bool
}

func (r telegramReceiver) drain(ctx context.Context, s telegramSettings) error {
	for range 100 {
		id, hash, raw, err := r.store.NextTelegramCallback(ctx, s.bot, s.source.channel.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var queued queuedTelegram
		if json.Unmarshal(raw, &queued) != nil {
			return errors.New("invalid queued Telegram update")
		}
		ip, ipErr := netip.ParseAddr(queued.Peer)
		msg := queued.Message.Message
		allowed := telegramSenderAllowed(s.source.config, msg.From.ID.String(), msg.Text)
		if hash == s.callbackSource && ipErr == nil && signinIPAllowed(objectValue(s.cfg["security"])["telegram_webhook_allow_ip"], ip) && (queued.Allowed || !allowed) {
			if err = r.dispatch(ctx, s, queued.Message); err != nil {
				return err
			}
		} else {
			// An old denial cannot turn into a command after a settings change.
			if _, err = r.store.ClaimTelegramUpdate(ctx, s.source.channel, s.bot, id); err != nil {
				return err
			}
		}
		if err = r.store.RemoveTelegramCallback(ctx, s.bot, id); err != nil {
			return err
		}
	}
	return nil
}

func (r telegramReceiver) poll(ctx context.Context, s telegramSettings) error {
	offset, err := r.store.TelegramOffset(ctx, s.bot, 0)
	if err != nil {
		return err
	}
	var result struct {
		OK     bool
		Result []json.RawMessage
	}
	params := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "limit": {"100"}, "timeout": {"5"}, "allowed_updates": {`["message"]`}}
	if err = r.call(ctx, s, "getUpdates", params, &result); err != nil {
		return err
	}
	if !result.OK || result.Result == nil || len(result.Result) > 100 {
		return errors.New("invalid Telegram updates")
	}
	for _, raw := range result.Result {
		var u struct {
			ID json.Number `json:"update_id"`
		}
		if err = json.Unmarshal(raw, &u); err != nil {
			return err
		}
		id, err := u.ID.Int64()
		if err != nil || id < 0 || id == math.MaxInt64 {
			return errors.New("invalid Telegram update id")
		}
		if id < offset {
			continue
		}
		if err = r.handoff(ctx, s, raw); err != nil {
			return err
		}
		offset = id + 1
		if err = r.store.SaveTelegramOffset(ctx, s.bot, offset); err != nil {
			return err
		}
	}
	return nil
}

// The helper exits when its owning process closes stdin. Only the selected,
// saved interactive bot can register a webhook or consume updates.
func TelegramReceiverCommand(parent context.Context, path string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, in); cancel() }()
	db := filepath.Join(filepath.Dir(path), "user.db")
	store, err := notificationconfig.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()
	r := telegramReceiver{store: store, settings: config.NewStore(path), out: json.NewEncoder(out)}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	confirmed := ""
	var checkAt, pruneAt, retryAt time.Time
	var retrySource string
	var reported bool
	for ctx.Err() == nil {
		if time.Now().After(pruneAt) {
			if err := store.PruneTelegramCallbacks(ctx); err != nil {
				return err
			}
			pruneAt = time.Now().Add(time.Hour)
		}
		s, err := r.snapshot(ctx)
		if err != nil && release != nil {
			release()
			release = nil
			confirmed = ""
		}
		if err == nil && release == nil {
			release, err = holdTelegramReceiver(ctx, db)
		}
		if err == nil {
			err = r.drain(ctx, s)
		}
		if err == nil && s.source.mode == "poll" && !signinIPAllowed(objectValue(s.cfg["security"])["telegram_webhook_allow_ip"], netip.MustParseAddr("127.0.0.1")) {
			err = errors.New("local Telegram polling source is not enabled")
		}
		waiting := retrySource == s.fingerprint && time.Now().Before(retryAt)
		if err == nil && !waiting && (confirmed != s.fingerprint || time.Now().After(checkAt)) {
			err = r.reconcile(ctx, s)
			if err == nil {
				confirmed = s.fingerprint
				checkAt = time.Now().Add(time.Minute)
			}
		}
		if err == nil && !waiting && s.source.mode == "poll" {
			err = r.poll(ctx, s)
		}
		delay := time.Second
		if err != nil {
			confirmed = ""
			delay = 5 * time.Second
			var quota telegramBackoff
			if errors.As(err, &quota) {
				retrySource = s.fingerprint
				retryAt = time.Now().Add(quota.wait)
			}
			if !reported && ctx.Err() == nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, organization.ErrBusy) {
				slog.Warn("Telegram receiver waiting for valid settings or a confirmed response")
				reported = true
			}
		} else if !waiting {
			reported = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	return nil
}

func (r telegramReceiver) webhook(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, 405, "需要消息回调")
		return
	}
	s, err := r.snapshot(req.Context())
	if err != nil || s.source.mode != "webhook" {
		writeAPIError(w, 409, 409, "Telegram 回调渠道未启用")
		return
	}
	security := objectValue(s.cfg["security"])
	key := text(security["api_key"])
	auth := strings.Fields(req.Header.Get("Authorization"))
	if !constant(req.URL.Query().Get("apikey"), key) && (len(auth) == 0 || !constant(auth[len(auth)-1], key)) {
		writeAPIError(w, 401, 401, "消息回调凭据无效")
		return
	}
	if !signinIPAllowed(security["telegram_webhook_allow_ip"], signinRemoteIP(req)) {
		writeAPIError(w, 403, 403, "消息来源不在允许范围内")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 256<<10))
	if err != nil {
		writeAPIError(w, 413, 413, "消息过大")
		return
	}
	if !json.Valid(raw) {
		writeAPIError(w, 400, 400, "消息格式无效")
		return
	}
	if u, ok := decodeTelegramUpdate(raw); ok {
		id, _ := u.ID.Int64()
		// Only the fields needed by the dispatcher enter the temporary inbox.
		allowed := telegramSenderAllowed(s.source.config, u.Message.From.ID.String(), u.Message.Text)
		if !allowed {
			slash := strings.HasPrefix(u.Message.Text, "/")
			u.Message.From.Name = ""
			u.Message.Text = "."
			if slash {
				u.Message.Text = "/"
			}
		}
		minimal, err := json.Marshal(queuedTelegram{Message: u, Peer: signinRemoteIP(req).String(), Allowed: allowed})
		if err != nil {
			writeAPIError(w, 400, 400, "消息格式无效")
			return
		}
		if err = r.store.QueueTelegram(req.Context(), s.source.channel, s.bot, id, s.callbackSource, minimal); err != nil {
			writeAPIError(w, 503, 503, "Telegram 回调暂不可用")
			return
		}
	}
	_, _ = io.WriteString(w, "Ok")
}
