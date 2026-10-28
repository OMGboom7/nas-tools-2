package mediaserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrWebhook = errors.New("invalid media webhook")
var ErrWebhookIgnored = errors.New("media webhook event is not a notification")
var ErrWebhookState = errors.New("media webhook authority or receipt changed")
var ErrWebhookMissing = errors.New("media webhook receipt not found")

const WebhookControlKey = "GoMediaWebhooks"
const WebhookBodyLimit = 1 << 20

// Only named notification fields survive normalization. Raw callbacks may carry
// server credentials, filesystem paths, account avatars, and arbitrary URLs.
type WebhookEvent struct {
	Version    int      `json:"version"`
	Server     string   `json:"server"`
	Action     string   `json:"action"`
	MediaType  string   `json:"mediaType"`
	ItemID     string   `json:"itemId"`
	Name       string   `json:"name"`
	User       string   `json:"user"`
	Client     string   `json:"client"`
	Device     string   `json:"device"`
	Address    string   `json:"address"`
	Overview   string   `json:"overview"`
	Percentage *float64 `json:"percentage,omitempty"`
	Image      string   `json:"image"`
	Link       string   `json:"link"`
}

func webhookIcon(server string) string {
	switch server {
	case "emby":
		return "https://emby.media/notificationicon.png"
	case "plex":
		return "https://www.plex.tv/wp-content/uploads/2022/04/new-logo-process-lines-gray.png"
	case "jellyfin":
		return "https://jellyfin.org/images/logo.svg"
	}
	return ""
}

var webhookActions = map[string]string{
	"library.new": "新入库", "system.webhooktest": "测试",
	"playback.start": "开始播放", "playback.stop": "停止播放",
	"user.authenticated": "登录成功", "user.authenticationfailed": "登录失败",
	"item.rate": "标记了",
}

func webhookText(v any, limit int) (string, error) {
	if v == nil {
		return "", nil
	}
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case json.Number:
		s = string(v)
	default:
		return "", ErrWebhook
	}
	if !utf8.ValidString(s) || len(s) > limit || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return "", ErrWebhook
	}
	return strings.TrimSpace(s), nil
}

// Reject duplicate keys, excessive nesting, trailing documents, and large
// containers before ordinary decoding can silently choose a different value.
func webhookJSON(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > WebhookBodyLimit || !utf8.Valid(raw) {
		return nil, ErrWebhook
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	count := 0
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		count++
		if depth > 20 || count > 20000 {
			return nil, ErrWebhook
		}
		t, err := d.Token()
		if err != nil {
			return nil, ErrWebhook
		}
		switch t {
		case json.Delim('{'):
			m := map[string]any{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || len(name) > 1024 {
					return nil, ErrWebhook
				}
				if _, exists := m[name]; exists {
					return nil, ErrWebhook
				}
				m[name], err = value(depth + 1)
				if err != nil {
					return nil, err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrWebhook
			}
			return m, nil
		case json.Delim('['):
			a := []any{}
			for d.More() {
				v, err := value(depth + 1)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrWebhook
			}
			return a, nil
		default:
			if _, bad := t.(json.Delim); bad {
				return nil, ErrWebhook
			}
			return t, nil
		}
	}
	v, err := value(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrWebhook
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrWebhook
	}
	return m, nil
}

func ParseWebhook(server string, raw []byte) (WebhookEvent, string, error) {
	e := WebhookEvent{Version: 1, Server: server, Image: webhookIcon(server)}
	if e.Image == "" {
		return e, "", ErrWebhook
	}
	m, err := webhookJSON(raw)
	if err != nil {
		return e, "", err
	}
	var failure error
	str := func(m map[string]any, key string, limit int) string {
		v, err := webhookText(m[key], limit)
		if err != nil {
			failure = err
		}
		return v
	}
	obj := func(m map[string]any, key string) map[string]any {
		if m[key] == nil {
			return map[string]any{}
		}
		v, ok := m[key].(map[string]any)
		if !ok {
			failure = ErrWebhook
		}
		return v
	}
	num := func(v any) float64 {
		s, err := webhookText(v, 100)
		if err != nil {
			failure = err
			return 0
		}
		if s == "" {
			return 0
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			failure = ErrWebhook
		}
		return n
	}
	percent := func(position, runtime any) {
		p, r := num(position), num(runtime)
		if r > 0 {
			n := math.Min(100, p/r*100)
			e.Percentage = &n
		}
	}
	name := func(item map[string]any, title, year, series, season, episode string) {
		e.Name = str(item, title, 2000)
		switch e.MediaType {
		case "TV":
			s, ep := str(item, season, 16), str(item, episode, 16)
			if s != "" || ep != "" {
				// Do not invent missing season or episode numbers.
				prefix := ""
				if s != "" {
					prefix += "S" + s
				}
				if ep != "" {
					prefix += "E" + ep
				}
				e.Name = strings.TrimSpace(str(item, series, 1000) + " " + prefix + " " + e.Name)
			} else if title != series {
				e.Name = strings.TrimSpace(str(item, series, 1000) + " " + e.Name)
			}
		case "MOV", "SHOW":
			if y := str(item, year, 16); y != "" {
				e.Name += " (" + y + ")"
			}
		}
	}
	identity := str(m, "EventId", 512)
	if identity == "" {
		identity = str(m, "NotificationId", 512)
	}
	switch server {
	case "emby":
		e.Action = str(m, "Event", 128)
		item, session, user := obj(m, "Item"), obj(m, "Session"), obj(m, "User")
		switch str(item, "Type", 64) {
		case "Episode":
			e.MediaType = "TV"
		case "Series", "Season":
			e.MediaType = "SHOW"
		case "Audio", "AudioBook", "MusicAlbum":
			e.MediaType = "AUD"
		case "Movie", "Video":
			e.MediaType = "MOV"
		}
		name(item, "Name", "ProductionYear", "SeriesName", "ParentIndexNumber", "IndexNumber")
		if e.Name == "" {
			e.Name = str(m, "Title", 2000)
		}
		e.ItemID = str(item, "Id", 256)
		e.Overview = str(item, "Overview", 20000)
		e.User, e.Client, e.Device, e.Address = str(user, "Name", 256), str(session, "Client", 256), str(session, "DeviceName", 256), str(session, "RemoteEndPoint", 512)
		if p := obj(m, "TranscodingInfo")["CompletionPercentage"]; p != nil {
			n := num(p)
			if n > 100 {
				failure = ErrWebhook
			}
			e.Percentage = &n
		} else {
			percent(obj(m, "PlaybackInfo")["PositionTicks"], item["RunTimeTicks"])
		}
	case "jellyfin":
		action := str(m, "NotificationType", 128)
		e.Action = map[string]string{"PlaybackStart": "playback.start", "PlaybackStop": "playback.stop", "ItemAdded": "library.new", "AuthenticationSuccess": "user.authenticated", "AuthenticationFailure": "user.authenticationfailed", "Test": "system.webhooktest"}[action]
		if e.Action == "" {
			e.Action = action
		}
		switch str(m, "ItemType", 64) {
		case "Episode":
			e.MediaType = "TV"
		case "Series", "Season":
			e.MediaType = "SHOW"
		case "Audio", "AudioBook", "MusicAlbum", "Album", "Song":
			e.MediaType = "AUD"
		case "Movie", "Video":
			e.MediaType = "MOV"
		}
		name(m, "Name", "Year", "SeriesName", "SeasonNumber", "EpisodeNumber")
		e.ItemID = str(m, "ItemId", 256)
		if e.ItemID == "" {
			e.ItemID = str(m, "Id", 256)
		}
		e.Overview = str(m, "Overview", 20000)
		e.User = str(m, "NotificationUsername", 256)
		if e.User == "" {
			e.User = str(m, "Username", 256)
		}
		e.Client = str(m, "ClientName", 256)
		if e.Client == "" {
			e.Client = str(m, "Client", 256)
		}
		e.Device, e.Address = str(m, "DeviceName", 256), str(m, "RemoteEndPoint", 512)
		percent(m["PlaybackPositionTicks"], m["RunTimeTicks"])
	case "plex":
		action := str(m, "event", 128)
		item, player, account := obj(m, "Metadata"), obj(m, "Player"), obj(m, "Account")
		if v := item["live"]; v != nil {
			if v == true || v == "1" || v == json.Number("1") {
				return e, "", ErrWebhookIgnored
			}
		}
		switch str(item, "type", 64) {
		case "episode":
			e.MediaType = "TV"
		case "movie":
			e.MediaType = "MOV"
		case "show":
			e.MediaType = "SHOW"
		default:
			if failure != nil {
				return e, "", failure
			}
			return e, "", ErrWebhookIgnored
		}
		e.Action = map[string]string{"media.play": "playback.start", "media.stop": "playback.stop", "library.new": "library.new"}[action]
		name(item, "title", "year", "grandparentTitle", "parentIndex", "index")
		e.ItemID = str(item, "ratingKey", 256)
		e.Overview = str(item, "summary", 20000)
		e.User, e.Client, e.Address = str(account, "title", 256), str(player, "title", 256), str(player, "publicAddress", 512)
		percent(item["viewOffset"], item["duration"])
	}
	if failure != nil {
		return e, "", failure
	}
	if webhookActions[e.Action] == "" {
		return e, "", ErrWebhookIgnored
	}
	if e.Action == "library.new" || e.Action == "playback.start" || e.Action == "playback.stop" || e.Action == "item.rate" {
		if e.Name == "" {
			return e, "", ErrWebhook
		}
	}
	// Match the old bounded synopsis without retaining the complete callback.
	runes := []rune(e.Overview)
	if len(runes) > 100 {
		e.Overview = string(runes[:100]) + "..."
	}
	if err := e.Validate(); err != nil {
		return e, "", err
	}
	return e, identity, nil
}

func (e WebhookEvent) Validate() error {
	if e.Version != 1 || webhookIcon(e.Server) == "" || e.Image != webhookIcon(e.Server) || webhookActions[e.Action] == "" {
		return ErrWebhook
	}
	for _, v := range []struct {
		s string
		n int
	}{{e.ItemID, 256}, {e.Name, 4096}, {e.User, 256}, {e.Client, 256}, {e.Device, 256}, {e.Address, 512}, {e.Overview, 1000}} {
		if _, err := webhookText(v.s, v.n); err != nil {
			return err
		}
	}
	if e.MediaType != "" && e.MediaType != "TV" && e.MediaType != "MOV" && e.MediaType != "SHOW" && e.MediaType != "AUD" {
		return ErrWebhook
	}
	if e.Percentage != nil && (math.IsNaN(*e.Percentage) || math.IsInf(*e.Percentage, 0) || *e.Percentage < 0 || *e.Percentage > 100) {
		return ErrWebhook
	}
	if e.Link != "" {
		u, err := url.Parse(e.Link)
		if err != nil || len(e.Link) > 4096 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Scheme != "http" && u.Scheme != "https" || strings.ContainsAny(e.Link, "\r\n") {
			return ErrWebhook
		}
	}
	return nil
}

func (e WebhookEvent) Message(received string) (string, string) {
	kind := map[string]string{"TV": "剧集", "SHOW": "剧集", "MOV": "电影", "AUD": "有声书"}[e.MediaType]
	title := strings.TrimSpace(webhookActions[e.Action] + kind + " " + e.Name)
	body := []string{"服务：" + e.Server}
	for _, v := range []struct{ label, value string }{{"用户", e.User}, {"客户端", e.Client}, {"设备", e.Device}, {"位置", e.Address}} {
		if v.value != "" {
			body = append(body, v.label+"："+v.value)
		}
	}
	if e.Percentage != nil {
		body = append(body, fmt.Sprintf("进度：%.2f%%", *e.Percentage))
	}
	if e.Overview != "" {
		body = append(body, "剧情："+e.Overview)
	}
	if t, err := time.Parse(time.RFC3339Nano, received); err == nil {
		body = append(body, "时间："+t.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	return title, strings.Join(body, "\n")
}
