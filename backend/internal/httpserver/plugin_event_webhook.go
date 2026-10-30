package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type eventWebhookAPI struct {
	organizer *organizationAPI
	system    *systemconfig.Store
	queue     *bus.Store
	transport http.RoundTripper
	workers   atomic.Int32
}

var eventWebhookKeys = []string{"UserInstalledPlugins", bus.EventWebhookKey, bus.EventWebhookControlKey}

const eventWebhookGuard = "native-event-webhook-outbox"

func (s pluginService) nativeEventWebhook(id string) bool {
	return s.eventWebhook != nil && strings.TrimSpace(id) == bus.EventWebhookPlugin
}
func (api *eventWebhookAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "native event webhook unavailable")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.queue == nil {
		writeAPIError(w, 503, 503, "native event webhook queue unavailable")
		return "", false
	}
	claims, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "current login required")
		return "", false
	}
	if _, err := api.authority(claims.Username); err != nil {
		writeAPIError(w, 403, 403, "current administrator authority required")
		return "", false
	}
	return claims.Username, true
}
func (api *eventWebhookAPI) authority(actor string) (string, error) {
	v, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	app := objectValue(v["app"])
	if actor == "" || text(app["login_user"]) != actor || !api.organizer.auth.service.IsAdministrator(actor) {
		return "", bus.ErrEventWebhook
	}
	return bus.Hash([]any{actor, app["login_password"], objectValue(v["security"])["api_key"]}), nil
}
func (api *eventWebhookAPI) fingerprint(raw map[string]string, actor string) string {
	authority, err := api.authority(actor)
	if err != nil {
		authority = "unavailable"
	}
	return bus.Hash([]any{raw, actor, authority})
}
func (api *eventWebhookAPI) detail(raw map[string]string, values map[string]any, actor string) pluginConfigDetail {
	spec, _ := bus.DecodeEventWebhookSpec(raw[bus.EventWebhookKey])
	if spec.Method == "" {
		spec.Method = "post"
	}
	c, _ := bus.DecodeEventWebhookControl(raw[bus.EventWebhookControlKey])
	return pluginConfigDetail{ID: "Webhook", Name: "Webhook 事件外发", Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}, Values: map[string]any{
		"enable": c.Authorized, "method": spec.Method, "get_encoding": spec.GetEncoding,
		"go_confirm": false, "go_payload": false, "go_old_stopped": false, "go_http": false,
	}, Fields: []pluginConfigField{
		{Key: "enable", Title: "接收授权后的新原生事件并外发", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "旧地址和安装状态不启用后台。保存新授权只处理以后产生的新事件，不补发历史或接管旧待办。"},
		{Key: "webhook_url", Title: "原事件接收地址", Type: "text", WriteOnly: true, Configured: spec.URL != "", Options: []pluginConfigChoice{}, Tooltip: "留空保留原值。完整地址、查询参数及 URL 凭据不会回显；向指定接收端发送事件数据。"},
		{Key: "method", Title: "请求方式", Type: "select", Options: []pluginConfigChoice{{Value: "post", Label: "POST JSON"}, {Value: "get", Label: "GET 查询参数"}}},
		{Key: "get_encoding", Title: "GET 数据兼容方式", Type: "select", Options: []pluginConfigChoice{{Value: "legacy", Label: "保留旧 Requests 字典键展开"}, {Value: "json", Label: "完整 JSON data 参数"}}, Tooltip: "旧 GET 实际只展开 data 字典的键；JSON 方式包含值，会改变接收端收到的参数，需明确选择。POST 总是 type/data JSON。"},
		{Key: "go_confirm", Title: "确认向当前原地址接收未来新事件", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_payload", Title: "允许传送事件中的媒体路径、站点链接及用户播放信息", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "按各原生生产者实际提供的字段发送；原始媒体回调可能包含额外私有字段。仅授权给上面指定的原接收地址。"},
		{Key: "go_old_stopped", Title: "确认旧 Python Webhook 插件及其他重复发送器已停止", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_http", Title: "可选：允许原接收地址使用明文 HTTP", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "HTTP 会在该连接上传输事件内容及 URL 凭据。HTTPS 不关闭证书验证；所有重定向均不跟随。"},
	}}
}
func (s pluginService) eventWebhookSummary(ctx context.Context) (bool, bool) {
	c, _, active, err := s.eventWebhook.queue.EventWebhookScope(ctx)
	if err != nil {
		return false, false
	}
	if !active {
		return false, true
	}
	authority, err := s.eventWebhook.authority(c.Actor)
	return err == nil && authority == c.Authority, true
}
func (s pluginService) serveEventWebhookPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.eventWebhook
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	raw, err := s.system.ReadMany(r.Context(), eventWebhookKeys)
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], "Webhook")
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	values, configErr := decodeMetadataConfig(raw[bus.EventWebhookKey])
	write := action == "install" || action == "uninstall" || action == "save" || action == "legacy-save"
	if !write {
		if !installed {
			writeAPIError(w, 404, 404, "event webhook plugin not installed")
			return
		}
		if action == "status" {
			active, known := s.eventWebhookSummary(r.Context())
			writeJSON(w, 200, map[string]any{"code": 0, "state": active, "stateKnown": known})
			return
		}
		if configErr != nil {
			eventWebhookError(w, configErr)
			return
		}
		if action == "page" {
			api.pluginPage(w, r)
			return
		}
		if action != "get" {
			writeAPIError(w, 405, 405, "unsupported event webhook action")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": api.detail(raw, values, actor)})
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 409, 409, "event webhook plugin not installed")
		return
	}
	if configErr != nil && action != "uninstall" {
		eventWebhookError(w, configErr)
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	next := values
	review := api.fingerprint(raw, actor)
	explicit := false
	switch action {
	case "save":
		var in pluginConfigRequest
		if !decodeServiceRequest(w, r, &in, "invalid event webhook settings") {
			return
		}
		if in.Fingerprint != review {
			eventWebhookError(w, bus.ErrEventWebhook)
			return
		}
		var message string
		next, message = mergePluginConfig(values, api.detail(raw, values, actor).Fields, in)
		if message != "" {
			writeAPIError(w, 400, 400, "invalid event webhook fields")
			return
		}
		explicit = true
	case "legacy-save":
		next, err = decodeMetadataConfig(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "invalid legacy event webhook configuration")
			return
		}
		explicit = r.PostForm.Get("fingerprint") == review
		if next["go_confirm"] == true && !explicit {
			eventWebhookError(w, bus.ErrEventWebhook)
			return
		}
	}
	enable := false
	if action == "save" || action == "legacy-save" {
		enable = next["enable"] == true && explicit && next["go_confirm"] == true && next["go_payload"] == true && next["go_old_stopped"] == true
		if action == "save" && next["enable"] == true && !enable {
			writeAPIError(w, 400, 400, "separate target, event data and stopped old sender consent required")
			return
		}
	}
	httpAllowed := next["go_http"] == true
	for _, key := range []string{"go_confirm", "go_payload", "go_old_stopped", "go_http"} {
		delete(next, key)
	}
	configRaw, err := json.Marshal(next)
	if err != nil || len(configRaw) > 64<<10 {
		eventWebhookError(w, bus.ErrEventWebhook)
		return
	}
	spec, err := bus.DecodeEventWebhookSpec(string(configRaw))
	if err != nil && action != "uninstall" {
		eventWebhookError(w, err)
		return
	}
	if enable && (spec.URL == "" || spec.Method == "") {
		writeAPIError(w, 400, 400, "complete original receiver URL and method required")
		return
	}
	u, _ := url.Parse(spec.URL)
	if enable && u.Scheme == "http" && !httpAllowed {
		writeAPIError(w, 400, 400, "separate plaintext HTTP event transmission consent required")
		return
	}
	epoch, err := refreshNonce()
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	c := bus.EventWebhookControl{Epoch: epoch, Actor: actor, Authority: authority, ConfigDigest: bus.Hash(next), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: enable && action != "uninstall", HTTP: httpAllowed}
	controlRaw, _ := json.Marshal(c)
	err = s.system.UpdateMany(r.Context(), eventWebhookKeys, func(current map[string]string) (map[string]string, error) {
		if bus.Hash(current) != bus.Hash(raw) || api.fingerprint(current, actor) != review {
			return nil, bus.ErrEventWebhook
		}
		ids, err := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if err != nil {
			return nil, err
		}
		if action == "install" {
			ids = append(ids, "Webhook")
		}
		if action == "uninstall" {
			kept := []string{}
			for _, id := range ids {
				if id != "Webhook" {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		installedRaw, _ := json.Marshal(ids)
		out := map[string]string{"UserInstalledPlugins": string(installedRaw), bus.EventWebhookControlKey: string(controlRaw)}
		if action != "uninstall" {
			out[bus.EventWebhookKey] = string(configRaw)
		}
		return out, nil
	})
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "active": c.Authorized, "message": "事件外发设置已保存；新授权不接管旧回执，响应丢失请先只读查回"})
}
func eventWebhookError(w http.ResponseWriter, err error) {
	if errors.Is(err, bus.ErrEventWebhookMissing) {
		writeAPIError(w, 404, 404, "event webhook receipt not found")
		return
	}
	writeAPIError(w, 409, 409, "original event webhook settings or receipt require review; no delivery retried")
}
func (api *eventWebhookAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	base := "/api/v1/event-webhooks"
	switch {
	case r.Method == "GET" && r.URL.Path == base:
		c, spec, active, err := api.queue.EventWebhookScope(r.Context())
		valid := false
		if err == nil && active {
			authority, e := api.authority(c.Actor)
			valid = e == nil && authority == c.Authority
		}
		origin := ""
		if u, e := url.Parse(spec.URL); e == nil && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"active": valid, "epoch": c.Epoch, "actor": c.Actor, "created": c.Created, "origin": origin, "method": spec.Method, "encoding": spec.GetEncoding, "current": err == nil, "workerRunning": api.workers.Load() > 0}})
		return
	case r.Method == "GET" && r.URL.Path == base+"/receipts":
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				eventWebhookError(w, bus.ErrEventWebhook)
				return
			}
			page = n
		}
		rows, err := api.queue.EventWebhooks(r.Context(), page, false)
		if err != nil {
			eventWebhookError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": rows})
		return
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, base+"/receipts/"):
		id := strings.TrimPrefix(r.URL.Path, base+"/receipts/")
		p, err := api.queue.EventWebhook(r.Context(), id)
		if err != nil {
			eventWebhookError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
		return
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/dismiss") && strings.HasPrefix(r.URL.Path, base+"/receipts/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/receipts/"), "/dismiss")
		var in struct {
			Confirm bool   `json:"confirm"`
			Reason  string `json:"reason"`
		}
		if !decodeOrganizationRequest(w, r, &in, "invalid original event disposition") {
			return
		}
		if !in.Confirm {
			eventWebhookError(w, bus.ErrEventWebhook)
			return
		}
		guard, err := api.organizer.jobs.AcquireJob(r.Context(), eventWebhookGuard)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		defer guard.Close()
		if err := api.queue.DismissEventWebhook(r.Context(), id, actor, in.Reason); err != nil {
			eventWebhookError(w, err)
			return
		}
		p, err := api.queue.EventWebhook(r.Context(), id)
		if err != nil {
			eventWebhookError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
		return
	}
	writeAPIError(w, 404, 404, "unknown native event webhook operation")
}
func (api *eventWebhookAPI) pluginPage(w http.ResponseWriter, r *http.Request) {
	rows, err := api.queue.EventWebhooks(r.Context(), 1, false)
	if err != nil {
		eventWebhookError(w, err)
		return
	}
	data := pluginPageData{Title: "Webhook 原生事件外发回执", ReadOnly: true, Sections: []string{
		"只读显示最近 30 条；插件页的稳定编号面板可查询较早回执及明确忽略待核对项。",
		"旧安装／旧地址不启用后台，新授权不补发历史。原目标／原配置变化或卸载会阻止旧待办；未知应答与重启不自动重发。",
		"原生已提交业务及媒体回调接入；其他事件随对应原生生产者逐步接通，不启动 Python 插件。",
	}, Tables: []pluginPageTable{{Columns: []string{"稳定编号", "事件类型", "原业务引用", "状态", "说明"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}}}
	for _, p := range rows {
		data.Tables[0].Rows = append(data.Tables[0].Rows, []string{p.ID, p.Type, p.Reference, p.State, p.Reason})
		data.Tables[0].RowActions = append(data.Tables[0].RowActions, nil)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api *eventWebhookAPI) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, eventWebhookGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	rows, err := api.queue.EventWebhooks(ctx, 1, true)
	if err != nil {
		return err
	}
	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, spec, err := api.queue.ClaimEventWebhook(ctx, p, func(c bus.EventWebhookControl) error {
			authority, err := api.authority(c.Actor)
			if err != nil || authority != c.Authority {
				return bus.ErrEventWebhook
			}
			return nil
		}, func(tx *sql.Tx, source bus.EventWebhookSource) error {
			return organization.SubtitleIntakeReady(ctx, tx, source.Reference, source.Ordinal, source.Origin, &source)
		})
		if errors.Is(err, bus.ErrEventWaiting) {
			if e := api.queue.WaitEventWebhook(ctx, p); e != nil {
				return e
			}
			continue
		}
		if errors.Is(err, bus.ErrEventWebhook) {
			continue
		}
		if err != nil {
			return err
		}
		status, sent := bus.SendEventWebhook(ctx, api.transport, spec, claimed)
		state := "uncertain"
		if sent {
			state = "sent"
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = api.queue.FinishEventWebhook(persist, claimed, state, status)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func startEventWebhookWorker(ctx context.Context, api *eventWebhookAPI) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	api.workers.Add(1)
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, cancel := context.WithTimeout(ctx, 6*time.Minute)
				err := api.tick(run)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("native event webhook dispatch interrupted; original receipts retained")
				}
			}
		}
	}()
	return func() { <-done }
}
