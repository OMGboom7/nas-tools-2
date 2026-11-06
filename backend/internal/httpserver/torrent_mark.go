package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

const markConfigKey = "plugin.TorrentMark"
const markStateKey = "GoTorrentMark"

type markSettings struct {
	Enable      bool     `json:"enable"`
	OnlyOnce    bool     `json:"onlyonce"`
	Cron        string   `json:"cron"`
	Downloaders []string `json:"downloaders"`
}
type markControl struct {
	Epoch, Actor, Scope, Next, Once string
	Exclusive                       bool
}
type markRun struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Created string `json:"created"`
	Changed int    `json:"changed"`
	Skipped int    `json:"skipped"`
	Unknown int    `json:"unknown"`
}
type markPending struct {
	ID         string `json:"id"`
	Downloader string `json:"downloader"`
	Hash       string `json:"hash"`
	Added      int64  `json:"added"`
	Goal       string `json:"goal"`
	Config     string `json:"-"`
	Labels     string `json:"-"`
}
type markStoredPending struct {
	markPending
	ConfigDigest, LabelsDigest string
}
type markState struct {
	Control markControl
	Runs    []markRun
	Pending []markStoredPending
}
type markScope struct {
	raw, installedRaw, actor, authority string
	installed                           bool
	settings                            markSettings
	targets                             []downloaderconfig.Downloader
	location                            *time.Location
}
type torrentMarkAPI struct {
	login       *siteLoginAPI
	system      *systemconfig.Store
	downloaders *downloaderconfig.Store
}
type markClient struct {
	ids   func(context.Context) ([]string, error)
	read  func(context.Context, string) (torrentmark.Evidence, error)
	write func(context.Context, torrentmark.Evidence, string) error
}

func parseMark(raw string) (markSettings, error) {
	var cfg markSettings
	if raw == "" {
		return cfg, nil
	}
	var fields map[string]json.RawMessage
	if len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return cfg, torrentmark.ErrEvidence
	}
	for key, target := range map[string]*bool{"enable": &cfg.Enable, "onlyonce": &cfg.OnlyOnce} {
		if value, ok := fields[key]; ok && json.Unmarshal(value, target) != nil {
			return cfg, torrentmark.ErrEvidence
		}
	}
	if value, ok := fields["cron"]; ok && json.Unmarshal(value, &cfg.Cron) != nil {
		return cfg, torrentmark.ErrEvidence
	}
	if value, ok := fields["downloaders"]; ok {
		var ids []json.RawMessage
		if json.Unmarshal(value, &ids) != nil || len(ids) > 16 {
			return cfg, torrentmark.ErrEvidence
		}
		seen := map[string]bool{}
		for _, id := range ids {
			var text string
			if json.Unmarshal(id, &text) != nil {
				text = string(id)
			}
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil || n <= 0 || strconv.FormatInt(n, 10) != text || seen[text] {
				return cfg, torrentmark.ErrEvidence
			}
			seen[text] = true
			cfg.Downloaders = append(cfg.Downloaders, text)
		}
	}
	cfg.Cron = strings.TrimSpace(cfg.Cron)
	return cfg, nil
}
func mergeMark(raw string, changes map[string]json.RawMessage) (string, error) {
	fields := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &fields) != nil || fields == nil) {
		return "", torrentmark.ErrEvidence
	}
	for key, value := range changes {
		switch key {
		case "enable", "onlyonce", "cron", "downloaders":
			fields[key] = value
		default:
			return "", torrentmark.ErrEvidence
		}
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	_, err = parseMark(string(b))
	return string(b), err
}
func readMarkState(raw string) (markState, error) {
	out := markState{Runs: []markRun{}, Pending: []markStoredPending{}}
	if raw != "" && (len(raw) > 2<<20 || json.Unmarshal([]byte(raw), &out) != nil) {
		return out, torrentmark.ErrEvidence
	}
	if len(out.Runs) > 30 || len(out.Pending) > 500 {
		return out, torrentmark.ErrEvidence
	}
	return out, nil
}
func (api *torrentMarkAPI) update(ctx context.Context, fn func(map[string]string, *markState) error, receipt ...string) error {
	keys := append([]string{"UserInstalledPlugins", markConfigKey, markStateKey}, receipt...)
	return api.system.UpdateMany(ctx, keys, func(values map[string]string) (map[string]string, error) {
		st, err := readMarkState(values[markStateKey])
		if err != nil {
			return nil, err
		}
		if err := fn(values, &st); err != nil {
			return nil, err
		}
		b, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		values[markStateKey] = string(b)
		// Preserve absent settings until an explicit plugin change creates them.
		out := map[string]string{markStateKey: string(b)}
		for _, key := range []string{"UserInstalledPlugins", markConfigKey} {
			if values[key] != "" {
				out[key] = values[key]
			}
		}
		for _, key := range receipt {
			if values[key] != "" {
				out[key] = values[key]
			}
		}
		return out, nil
	})
}
func (api *torrentMarkAPI) scope(ctx context.Context, actor string) (markScope, error) {
	s := markScope{actor: actor}
	var err error
	s.authority, _, err = api.login.authority(actor)
	if err != nil {
		return s, err
	}
	values, err := api.system.ReadMany(ctx, []string{"UserInstalledPlugins", markConfigKey})
	if err != nil {
		return s, err
	}
	s.raw, s.installedRaw = values[markConfigKey], values["UserInstalledPlugins"]
	list, err := decodeInstalledPlugins(s.installedRaw)
	if err != nil {
		return s, err
	}
	s.installed = slices.Contains(list, "TorrentMark")
	s.settings, err = parseMark(s.raw)
	if err != nil {
		return s, err
	}
	s.location, err = rssWorkerLocation()
	if err != nil {
		return s, err
	}
	for _, id := range s.settings.Downloaders {
		n, _ := strconv.ParseInt(id, 10, 64)
		item, err := api.downloaders.Get(ctx, n)
		if err != nil || item.Enabled != 1 || (item.Type != "qbittorrent" && item.Type != "transmission") {
			return s, torrentmark.ErrEvidence
		}
		s.targets = append(s.targets, item)
	}
	return s, nil
}
func (s markScope) fingerprint() string {
	return organization.Digest([]any{"torrent-mark-v1", s.actor, s.authority, s.raw, s.installed, s.targets, s.location.String()})
}
func (api *torrentMarkAPI) state(ctx context.Context) (markState, error) {
	raw, err := api.system.Get(ctx, markStateKey)
	if err != nil {
		return markState{}, err
	}
	return readMarkState(raw)
}
func (api *torrentMarkAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.system == nil || api.login == nil {
		writeAPIError(w, 503, 503, "种子标记不可用")
		return "", false
	}
	actor, ok := api.login.actor(w, r, false)
	if ok && write && !api.login.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式执行种子标记")
		return "", false
	}
	return actor, ok
}
func markError(w http.ResponseWriter, err error) {
	if errors.Is(err, organization.ErrBusy) {
		writeAPIError(w, 409, 409, "原标记正在执行，请核对记录")
		return
	}
	writeAPIError(w, 409, 409, "种子标记设置、任务或执行状态已变化，请重新读取")
}

type markInput struct {
	Fingerprint string `json:"fingerprint"`
	Epoch       string `json:"epoch"`
	RequestID   string `json:"requestId"`
	Confirm     bool   `json:"confirm"`
	Exclusive   bool   `json:"exclusive"`
}

func (api *torrentMarkAPI) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	ctx := r.Context()
	st, err := api.state(ctx)
	if err != nil {
		markError(w, err)
		return
	}
	var in markInput
	if r.Method != http.MethodGet && !siteLoginJSON(w, r, &in) {
		return
	}
	action := r.PathValue("action")
	if action == "stop" {
		if !in.Confirm || in.Epoch != st.Control.Epoch {
			markError(w, torrentmark.ErrEvidence)
			return
		}
		err = api.update(ctx, func(_ map[string]string, current *markState) error {
			if current.Control != st.Control {
				return torrentmark.ErrEvidence
			}
			current.Control = markControl{Epoch: sitelogin.Random()}
			for i := range current.Runs {
				if current.Runs[i].State == "running" {
					current.Runs[i].State = "stopped"
				}
			}
			return nil
		})
		if err != nil {
			markError(w, err)
			return
		}
		signinJSON(w, map[string]bool{"stopped": true})
		return
	}
	id := ""
	if action == "run" {
		if !sitelogin.Hex(in.RequestID, 64) {
			markError(w, torrentmark.ErrEvidence)
			return
		}
		id = organization.Digest([]string{"torrent-mark", actor, in.RequestID})
		for _, run := range st.Runs {
			if run.ID == id {
				signinJSON(w, run)
				return
			}
		}
		seen, err := api.system.Get(ctx, markReceiptKey(id))
		if err != nil {
			markError(w, err)
			return
		}
		if seen != "" {
			signinJSON(w, markRun{ID: id, Kind: "manual", State: "archived"})
			return
		}
	}
	s, err := api.scope(ctx, actor)
	if err != nil || !s.installed {
		markError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		targets := []map[string]any{}
		exclusive := false
		for _, item := range s.targets {
			var cfg map[string]any
			_ = json.Unmarshal([]byte(item.Config), &cfg)
			host := text(cfg["host"])
			if !strings.Contains(host, "://") {
				host = "http://" + host
			}
			u, _ := url.Parse(host)
			receiver := ""
			if u != nil && u.User == nil {
				if port := text(cfg["port"]); port != "" {
					if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
						u.Host = net.JoinHostPort(u.Hostname(), port)
					}
				}
				receiver = u.Scheme + "://" + u.Host
			}
			targets = append(targets, map[string]any{"id": strconv.FormatInt(item.ID, 10), "name": item.Name, "type": item.Type, "receiver": receiver})
			exclusive = exclusive || item.Type == "transmission"
		}
		pending := []markPending{}
		for _, p := range st.Pending {
			pending = append(pending, p.markPending)
		}
		signinJSON(w, map[string]any{"fingerprint": s.fingerprint(), "settings": s.settings, "control": st.Control, "active": st.Control.Scope == s.fingerprint() && (st.Control.Next != "" || st.Control.Once != ""), "goOnly": api.login.pureGo, "targets": targets, "exclusiveRequired": exclusive, "runs": st.Runs, "pending": pending})
		return
	}
	if !in.Confirm || in.Fingerprint != s.fingerprint() {
		markError(w, torrentmark.ErrEvidence)
		return
	}
	for _, item := range s.targets {
		if action != "reconcile" && item.Type == "transmission" && !in.Exclusive {
			markError(w, torrentmark.ErrEvidence)
			return
		}
	}
	switch action {
	case "activate":
		if !s.settings.Enable || len(s.targets) == 0 {
			markError(w, torrentmark.ErrEvidence)
			return
		}
		next, err := markSchedule(s, time.Now())
		if err != nil {
			markError(w, err)
			return
		}
		updated := s.raw
		c := markControl{Epoch: sitelogin.Random(), Actor: actor, Exclusive: in.Exclusive}
		if !next.IsZero() {
			c.Next = next.UTC().Format(time.RFC3339Nano)
		}
		if s.settings.OnlyOnce {
			updated, err = mergeMark(s.raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false")})
			if err != nil {
				markError(w, err)
				return
			}
			c.Once = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
		}
		if c.Next == "" && c.Once == "" {
			markError(w, torrentmark.ErrEvidence)
			return
		}
		original := s.raw
		s.raw = updated
		c.Scope = s.fingerprint()
		err = api.update(ctx, func(values map[string]string, current *markState) error {
			if values[markConfigKey] != original || values["UserInstalledPlugins"] != s.installedRaw || current.Control.Epoch != in.Epoch {
				return torrentmark.ErrEvidence
			}
			for _, run := range current.Runs {
				if run.State == "running" {
					return organization.ErrBusy
				}
			}
			values[markConfigKey] = updated
			current.Control = c
			return nil
		})
		if err != nil {
			markError(w, err)
			return
		}
		signinJSON(w, c)
	case "run":
		run, err := api.execute(ctx, s, id, "manual", in.Exclusive, "", "")
		if err != nil {
			markError(w, err)
			return
		}
		signinJSON(w, run)
	case "reconcile":
		if err := api.reconcile(ctx, s); err != nil {
			markError(w, err)
			return
		}
		signinJSON(w, map[string]bool{"checked": true})
	default:
		writeAPIError(w, 404, 404, "未找到标记操作")
	}
}
