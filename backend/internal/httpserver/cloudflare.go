package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/cloudflare"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func cfError(w http.ResponseWriter, err error) {
	writeAPIError(w, 409, 409, "原配置、任务或 Hosts 结果需要核对；没有重复测速或写入")
}
func (api *cloudflareAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.organizer == nil || api.organizer.auth == nil {
		writeAPIError(w, 503, 503, "CF 优选服务不可用")
		return "", false
	}
	claims, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "需要当前登录")
		return "", false
	}
	if !api.organizer.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "需要当前管理员")
		return "", false
	}
	if write && !api.organizer.pureGo {
		writeAPIError(w, 501, 501, "原生优选需要关闭旧后端")
		return "", false
	}
	return claims.Username, true
}
func (api *cloudflareAPI) fingerprint(ctx context.Context, actor string) (string, error) {
	src, err := api.store.Source(ctx)
	if err != nil {
		return "", err
	}
	authority, err := api.authority(actor)
	if err != nil {
		return "", err
	}
	return api.fingerprintSource(actor, src, authority), nil
}
func (api *cloudflareAPI) fingerprintSource(actor string, src cloudflare.Source, authority string) string {
	path, fileHash := "", "unavailable"
	if api.hosts != nil {
		path = api.hosts.path
		if raw, e := cfReadFile(path); e == nil {
			fileHash = cfHash(raw)
		}
	}
	return bus.Hash([]any{src, actor, authority, api.root, path, fileHash})
}
func cfValues(s cloudflare.Settings) map[string]any {
	return map[string]any{"cf_ip": s.CFIP, "cron": s.Cron, "version": s.Version, "additional_args": s.AdditionalArgs, "ipv4": s.IPv4, "ipv6": s.IPv6, "check": s.Check, "re_install": s.Reinstall, "notify": s.Notify, "onlyonce": s.Once}
}
func cfJobView(j cloudflare.Job) map[string]any {
	out := map[string]any{"id": j.ID, "origin": j.Origin, "state": j.State, "reason": j.Reason, "created": j.Created, "updated": j.Updated, "canReconcile": j.State == "unknown" && j.Plan != nil, "canClose": j.State == "unknown"}
	if j.Result != nil {
		out["bestIP"] = j.Result.BestIP
		out["version"] = j.Result.Version
	}
	if j.Plan != nil {
		out["beforeIP"] = j.Plan.Hosts.BeforeIP
		out["afterIP"] = j.Plan.Hosts.AfterIP
	}
	return out
}
func (api *cloudflareAPI) status(ctx context.Context, actor string) (map[string]any, error) {
	src, err := api.store.Source(ctx)
	if err != nil {
		return nil, err
	}
	s, err := cloudflare.DecodeSettings(src.Config)
	if err != nil {
		return nil, err
	}
	c, err := api.store.Control(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := api.store.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	authority, err := api.authority(actor)
	if err != nil {
		return nil, err
	}
	basis := api.fingerprintSource(actor, src, authority)
	if err != nil {
		return nil, err
	}
	views := []map[string]any{}
	for _, j := range jobs {
		views = append(views, cfJobView(j))
	}
	ready := api.hosts != nil && api.organizer.pureGo && cloudflare.Installed(src)
	if ready {
		_, e := cfReadFile(api.hosts.path)
		ready = e == nil
	}
	return map[string]any{"fingerprint": basis, "ready": ready, "enabled": c.Enabled, "current": api.validate(ctx, c) == nil, "fromSignin": c.FromSignin, "nextRun": c.NextRun, "nextOnce": c.NextOnce, "values": cfValues(s), "jobs": views}, nil
}
func (api *cloudflareAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	ctx := r.Context()
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/cloudflare"), "/")
	if r.Method == http.MethodGet {
		if strings.HasPrefix(action, "jobs/") {
			j, err := api.store.Get(ctx, strings.TrimPrefix(action, "jobs/"))
			if err != nil {
				cfError(w, err)
				return
			}
			signinJSON(w, cfJobView(j))
			return
		}
		if action != "" {
			writeAPIError(w, 404, 404, "未找到优选入口")
			return
		}
		v, err := api.status(ctx, actor)
		if err != nil {
			cfError(w, err)
			return
		}
		signinJSON(w, v)
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, 405, "不支持此优选操作")
		return
	}
	var in struct {
		Fingerprint, Nonce, ID string
		FromSignin, Confirm    bool
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if action == "stop" {
		err := api.store.Stop(ctx)
		api.cancelRun()
		if err != nil {
			cfError(w, err)
			return
		}
		signinJSON(w, map[string]any{"stopped": true})
		return
	}
	if action == "reconcile" || action == "close" {
		guard, err := api.organizer.jobs.AcquireJob(ctx, cloudflareGuard)
		if err != nil {
			cfError(w, err)
			return
		}
		defer guard.Close()
		if action == "close" {
			if !in.Confirm {
				cfError(w, cloudflare.ErrState)
				return
			}
			err = api.store.Terminal(ctx, in.ID, "closed", "管理员确认关闭本地未决占用；未重发测速、未写入文件")
		} else {
			err = api.reconcile(ctx, in.ID)
		}
		if err != nil {
			cfError(w, err)
			return
		}
		j, err := api.store.Get(ctx, in.ID)
		if err != nil {
			cfError(w, err)
			return
		}
		signinJSON(w, cfJobView(j))
		return
	}
	if action == "run" {
		if len(in.Nonce) < 16 || len(in.Nonce) > 128 || strings.ContainsAny(in.Nonce, " \n\r\t") {
			cfError(w, cloudflare.ErrState)
			return
		}
		origin := "manual:" + actor + ":" + in.Nonce
		if old, err := api.store.Get(ctx, bus.Hash(origin)); err == nil {
			signinJSON(w, cfJobView(old))
			return
		}
	}
	reviewSource, err := api.store.Source(ctx)
	if err != nil {
		cfError(w, err)
		return
	}
	reviewAuthority, err := api.authority(actor)
	if err != nil {
		cfError(w, err)
		return
	}
	basis := api.fingerprintSource(actor, reviewSource, reviewAuthority)
	if action == "review" {
		status, err := api.status(ctx, actor)
		if err != nil {
			cfError(w, err)
			return
		}
		if len(in.Nonce) >= 16 && len(in.Nonce) <= 128 {
			status["requestID"] = bus.Hash("manual:" + actor + ":" + in.Nonce)
		}
		signinJSON(w, status)
		return
	}
	if !in.Confirm || in.Fingerprint != basis {
		cfError(w, cloudflare.ErrState)
		return
	}
	if action == "activate" {
		src := reviewSource
		if api.hosts == nil {
			cfError(w, cloudflare.ErrState)
			return
		}
		if _, err = cfReadFile(api.hosts.path); err != nil {
			cfError(w, err)
			return
		}
		s, err := cloudflare.DecodeSettings(src.Config)
		if err != nil {
			cfError(w, err)
			return
		}
		loc, err := rssWorkerLocation()
		if err != nil {
			cfError(w, err)
			return
		}
		authority, err := api.authority(actor)
		if err != nil || authority != reviewAuthority || api.fingerprintSource(actor, src, authority) != basis {
			cfError(w, err)
			return
		}
		c := cloudflare.Control{Epoch: sitelogin.Random(), Actor: actor, Authority: authority, Root: api.root, HostsPath: api.hosts.path, Location: loc.String(), Enabled: true, FromSignin: in.FromSignin}
		if s.Cron != "" {
			if len(strings.Fields(s.Cron)) != 5 {
				cfError(w, cloudflare.ErrState)
				return
			}
			schedule, e := parseRSSSchedule(s.Cron, loc)
			if e != nil {
				cfError(w, e)
				return
			}
			c.NextRun = schedule.next(time.Now()).UTC().Format(time.RFC3339Nano)
		}
		if s.Once {
			c.NextOnce = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
		}
		if err = api.store.Activate(ctx, c, src); err != nil {
			cfError(w, err)
			return
		}
		api.cancelRun()
		signinJSON(w, map[string]any{"enabled": true})
		return
	}
	if action == "run" {
		c, err := api.store.Control(ctx)
		if err != nil || api.validate(ctx, c) != nil {
			cfError(w, cloudflare.ErrState)
			return
		}
		j, err := api.store.Queue(ctx, "manual:"+actor+":"+in.Nonce, false, reviewSource)
		if err != nil {
			cfError(w, err)
			return
		}
		signinJSON(w, cfJobView(j))
		return
	}
	writeAPIError(w, 404, 404, "未找到优选操作")
}

func (s pluginService) nativeCloudflare(id string) bool {
	return id == cloudflare.Plugin && s.cloudflare != nil && s.cloudflare.store != nil
}
func (api *cloudflareAPI) detail(ctx context.Context, actor string) (pluginConfigDetail, error) {
	src, err := api.store.Source(ctx)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	settings, err := cloudflare.DecodeSettings(src.Config)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	authority, err := api.authority(actor)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	basis := api.fingerprintSource(actor, src, authority)
	fields := []pluginConfigField{}
	for _, p := range []struct{ key, title string }{{"cf_ip", "当前优选 IP"}, {"cron", "优选周期（五段 cron）"}, {"additional_args", "原高级测速参数"}, {"version", "已安装版本"}} {
		fields = append(fields, pluginConfigField{Key: p.key, Title: p.title, Type: "text", ReadOnly: p.key == "version", WriteOnly: p.key != "version", Configured: configuredPluginValue(cfValues(settings)[p.key]), Options: []pluginConfigChoice{}})
	}
	for _, p := range []struct{ key, title string }{{"ipv4", "IPv4"}, {"ipv6", "IPv6"}, {"check", "按唯一多数地址校准"}, {"onlyonce", "接管后运行一次"}, {"re_install", "重装后运行"}, {"notify", "完成时发送插件通知"}} {
		fields = append(fields, pluginConfigField{Key: p.key, Title: p.title, Type: "switch", Options: []pluginConfigChoice{}})
	}
	return pluginConfigDetail{ID: cloudflare.Plugin, Name: "Cloudflare IP 优选", Fields: fields, Values: cfValues(settings), Meta: pluginConfigDetailMeta{Fingerprint: basis}}, nil
}
func (s pluginService) serveCloudflarePlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.cloudflare
	actor, ok := api.actor(w, r, action != "get" && action != "status")
	if !ok {
		return
	}
	src, err := api.store.Source(r.Context())
	if err != nil {
		cfError(w, err)
		return
	}
	installed, _ := indexerInstalled(src.Installed, cloudflare.Plugin)
	if !installed && action != "install" && action != "uninstall" {
		writeAPIError(w, 404, 404, "优选插件未安装")
		return
	}
	if action == "get" {
		detail, err := api.detail(r.Context(), actor)
		if err != nil {
			cfError(w, err)
			return
		}
		signinJSON(w, detail)
		return
	}
	if action == "status" {
		c, err := api.store.Control(r.Context())
		writeJSON(w, 200, map[string]any{"code": 0, "state": installed && err == nil && api.validate(r.Context(), c) == nil, "stateKnown": err == nil})
		return
	}
	next := src.Config
	var installation *bool
	if action == "install" || action == "uninstall" {
		value := action == "install"
		installation = &value
	} else if action == "save" {
		var in struct {
			Fingerprint string
			Values      map[string]json.RawMessage
			ClearConfig []string
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		authority, err := api.authority(actor)
		basis := api.fingerprintSource(actor, src, authority)
		if err != nil || basis != in.Fingerprint {
			cfError(w, cloudflare.ErrState)
			return
		}
		if in.Values == nil {
			in.Values = map[string]json.RawMessage{}
		}
		for _, key := range in.ClearConfig {
			in.Values[key] = json.RawMessage(`null`)
		}
		if _, exists := in.Values["version"]; exists {
			delete(in.Values, "version")
		}
		next, err = cloudflare.Patch(src.Config, in.Values)
	} else if action == "legacy-save" {
		var before, updates map[string]json.RawMessage
		if src.Config == "" {
			before = map[string]json.RawMessage{}
		} else if json.Unmarshal([]byte(src.Config), &before) != nil {
			cfError(w, cloudflare.ErrState)
			return
		}
		if json.Unmarshal([]byte(r.PostForm.Get("config")), &updates) != nil || updates == nil {
			cfError(w, cloudflare.ErrState)
			return
		}
		for key, value := range updates {
			before[key] = value
		}
		raw, e := json.Marshal(before)
		if e != nil {
			cfError(w, e)
			return
		}
		next = string(raw)
	} else {
		writeAPIError(w, 405, 405, "不支持此优选配置操作")
		return
	}
	if err == nil {
		err = api.store.Configure(r.Context(), src, next, installation)
	}
	if err != nil {
		cfError(w, err)
		return
	}
	api.cancelRun()
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "msg": "优选配置已保存，请核对后接管"})
}
