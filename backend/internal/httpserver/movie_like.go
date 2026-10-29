package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/movielike"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"gopkg.in/yaml.v3"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type movieLikeAPI struct {
	organizer *organizationAPI
	system    *systemconfig.Store
	queue     *movielike.Store
	transport http.RoundTripper
	workers   atomic.Int32
}

const movieLikeGuard = "native-movie-like"

var movieLikeKeys = []string{"UserInstalledPlugins", movielike.ConfigKey, movielike.ControlKey}

func (s pluginService) nativeMovieLike(id string) bool {
	return s.movieLike != nil && strings.TrimSpace(id) == movielike.Plugin
}
func (api *movieLikeAPI) authority(actor string) (string, error) {
	return (&eventWebhookAPI{organizer: api.organizer}).authority(actor)
}
func (api *movieLikeAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "媒体电影精选不可用")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.queue == nil {
		writeAPIError(w, 503, 503, "电影精选回执不可用")
		return "", false
	}
	c, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "需要当前登录")
		return "", false
	}
	if _, err := api.authority(c.Username); err != nil {
		writeAPIError(w, 403, 403, "需要当前管理员")
		return "", false
	}
	return c.Username, true
}
func (api *movieLikeAPI) appDigest() (string, error) {
	v, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	return bus.Hash(v), nil
}
func (api *movieLikeAPI) client() (*mediaserver.Client, error) {
	s, err := (&mediaRefreshAPI{organizer: api.organizer, connection: mediaSyncAPI{transport: api.transport}}).spec()
	if err != nil || s.kind != "emby" || s.cloud || s.client == nil {
		return nil, movielike.ErrState
	}
	return s.client, nil
}
func (api *movieLikeAPI) categories() (string, []string, error) {
	root, err := api.organizer.recognition.categories.load()
	if err != nil {
		return "", nil, err
	}
	names := []string{}
	if len(root.Content) > 0 {
		document, err := categoryNode(root.Content[0])
		if err != nil || document.Kind != yaml.MappingNode {
			return "", nil, movielike.ErrState
		}
		for i := 0; i+1 < len(document.Content); i += 2 {
			if document.Content[i].Value != "movie" {
				continue
			}
			section, err := categoryNode(document.Content[i+1])
			if err != nil || section.Kind != yaml.MappingNode {
				return "", nil, movielike.ErrState
			}
			for k := 0; k+1 < len(section.Content); k += 2 {
				names = append(names, section.Content[k].Value)
			}
		}
	}
	if len(names) == 0 || len(names) > 1000 {
		return "", nil, movielike.ErrState
	}
	for _, name := range names {
		if !movielike.Component(name) {
			return "", nil, movielike.ErrState
		}
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		return "", nil, err
	}
	return movielike.Hash(string(encoded)), names, nil
}
func (api *movieLikeAPI) movieRoots(ctx context.Context) ([]organizationRoot, error) {
	roots, err := api.organizer.roots(ctx)
	if err != nil {
		return nil, err
	}
	out := []organizationRoot{}
	for _, root := range roots.Targets {
		if root.Type == "MOV" {
			out = append(out, root)
		}
	}
	return out, nil
}
func (api *movieLikeAPI) validate(c movielike.Control) error {
	authority, err := api.authority(c.Actor)
	if err != nil || authority != c.Authority {
		return movielike.ErrState
	}
	app, err := api.appDigest()
	if err != nil || app != c.AppDigest {
		return movielike.ErrState
	}
	category, names, err := api.categories()
	if err != nil || category != c.CategoryDigest || movielike.Hash(names) != movielike.Hash(c.Categories) {
		return movielike.ErrState
	}
	if _, err := api.client(); err != nil {
		return err
	}
	roots, err := api.movieRoots(context.Background())
	if err != nil {
		return err
	}
	if len(c.Roots) == 0 {
		return movielike.ErrState
	}
	for _, selected := range c.Roots {
		found := false
		for _, root := range roots {
			if root.ID != selected.ID {
				continue
			}
			current, err := libraryRootSnapshot(root)
			if err != nil || current.Path != selected.Path || current.Identity != selected.Identity {
				return movielike.ErrState
			}
			found = true
		}
		if !found {
			return movielike.ErrState
		}
	}
	return nil
}
func (api *movieLikeAPI) fingerprint(raw map[string]string, actor string) string {
	authority, err := api.authority(actor)
	if err != nil {
		authority = "unavailable"
	}
	app, err := api.appDigest()
	if err != nil {
		app = "unavailable"
	}
	category, _, err := api.categories()
	if err != nil {
		category = "unavailable"
	}
	return bus.Hash([]any{raw, actor, authority, app, category})
}
func (s pluginService) movieLikeSummary(ctx context.Context) (bool, bool) {
	if s.movieLike.queue == nil {
		return false, true
	}
	c, _, on, err := s.movieLike.queue.Scope(ctx)
	if err != nil {
		return false, false
	}
	return on && s.movieLike.validate(c) == nil, true
}
func (api *movieLikeAPI) detail(raw map[string]string, actor string) (pluginConfigDetail, error) {
	values, err := movielike.Values(raw[movielike.ConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	fields := []pluginConfigField{
		{Key: "enable", Title: "准备启用电影精选", Type: "switch", Tooltip: "保存后保持自动移动停止，须在下方独立审核并授权未来 Emby 电影 item.rate 回调。", Options: []pluginConfigChoice{}},
		{Key: "dir_name", Title: "精选分类目录名称", Type: "text", Tooltip: "完整电影目录及所有附件一起移动；已有同名目标不覆盖、不合并。", Options: []pluginConfigChoice{}},
	}
	declared := map[string]any{"enable": values["enable"] == true, "dir_name": text(values["dir_name"])}
	for i, suffix := range []string{"", "2", "3"} {
		for _, field := range []struct{ key, label string }{{"local_path", "本机路径"}, {"remote_path", "Emby 路径"}} {
			key := field.key + suffix
			fields = append(fields, pluginConfigField{Key: key, Title: "路径映射 " + strconv.Itoa(i+1) + " · " + field.label, Type: "text", Tooltip: "原三组顺序生效，仅替换完整前导路径组件。每组两端同时填写或同时留空。", Options: []pluginConfigChoice{}})
			declared[key] = text(values[key])
		}
	}
	return pluginConfigDetail{ID: movielike.Plugin, Name: "电影精选", Fields: fields, Values: declared, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}}, nil
}
func movieLikeError(w http.ResponseWriter, err error) {
	if errors.Is(err, movielike.ErrMissing) {
		writeAPIError(w, 404, 404, "未找到原电影精选审核或回执")
		return
	}
	writeAPIError(w, 409, 409, "原电影精选配置或完整电影目录范围需要核对；没有自动重做，请只读查回原编号")
}
func (s pluginService) serveMovieLikePlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.movieLike
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	raw, err := api.system.ReadMany(r.Context(), movieLikeKeys)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], movielike.Plugin)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 404, 404, "电影精选插件未安装")
		return
	}
	if action == "get" {
		v, err := api.detail(raw, actor)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if action == "status" {
		on, known := s.movieLikeSummary(r.Context())
		writeJSON(w, 200, map[string]any{"code": 0, "state": on, "stateKnown": known})
		return
	}
	if action == "page" {
		api.pluginPage(w, r)
		return
	}
	if action != "install" && action != "uninstall" && action != "save" && action != "legacy-save" {
		writeAPIError(w, 405, 405, "不支持此电影精选操作")
		return
	}
	values, err := movielike.Values(raw[movielike.ConfigKey])
	if err != nil && action != "uninstall" {
		movieLikeError(w, err)
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	fingerprint := api.fingerprint(raw, actor)
	if action == "save" {
		var in pluginConfigRequest
		if !decodeServiceRequest(w, r, &in, "无效电影精选设置") {
			return
		}
		if in.Fingerprint != fingerprint {
			movieLikeError(w, movielike.ErrState)
			return
		}
		detail, err := api.detail(raw, actor)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		next, message := mergePluginConfig(values, detail.Fields, in)
		if message != "" {
			writeAPIError(w, 400, 400, "电影精选字段无效")
			return
		}
		values = next
	}
	if action == "legacy-save" {
		values, err = movielike.Values(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "无效原电影精选配置")
			return
		}
	}
	if action == "install" && text(values["dir_name"]) == "" {
		values["dir_name"] = "精选"
	}
	encoded, _ := json.Marshal(values)
	if action != "uninstall" {
		if _, err := movielike.ParseSpec(string(encoded)); err != nil {
			writeAPIError(w, 400, 400, "电影精选开关、目录名或路径映射无效")
			return
		}
	}
	epoch, err := refreshNonce()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	app, err := api.appDigest()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	digest := movielike.Hash(string(encoded))
	if digest == "" {
		digest = bus.Hash("retained invalid old config")
	}
	c := movielike.Control{Epoch: epoch, Actor: actor, Authority: authority, AppDigest: app, ConfigDigest: digest, Created: time.Now().UTC().Format(time.RFC3339Nano), Roots: []movielike.Root{}}
	control, _ := json.Marshal(c)
	err = api.system.UpdateMany(r.Context(), movieLikeKeys, func(current map[string]string) (map[string]string, error) {
		if bus.Hash(current) != bus.Hash(raw) || api.fingerprint(current, actor) != fingerprint {
			return nil, movielike.ErrState
		}
		ids, err := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if err != nil {
			return nil, err
		}
		if action == "install" {
			ids = append(ids, movielike.Plugin)
		}
		if action == "uninstall" {
			kept := []string{}
			for _, id := range ids {
				if id != movielike.Plugin {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		all, _ := json.Marshal(ids)
		out := map[string]string{"UserInstalledPlugins": string(all), movielike.ControlKey: string(control)}
		if action != "uninstall" {
			out[movielike.ConfigKey] = string(encoded)
		}
		return out, nil
	})
	if err != nil {
		movieLikeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "设置已保存，未来电影精选保持停止；请单独审核并授权"})
}

type movieLikeStatus struct {
	Installed     bool               `json:"installed"`
	Active        bool               `json:"active"`
	Current       bool               `json:"current"`
	Ready         bool               `json:"ready"`
	WorkerRunning bool               `json:"workerRunning"`
	Fingerprint   string             `json:"fingerprint"`
	Reason        string             `json:"reason"`
	Spec          movielike.Spec     `json:"spec"`
	Roots         []organizationRoot `json:"roots"`
	Categories    []string           `json:"categories"`
	Basis         string             `json:"-"`
}

func (api *movieLikeAPI) status(ctx context.Context, actor string) (movieLikeStatus, error) {
	v := movieLikeStatus{Roots: []organizationRoot{}, Categories: []string{}, WorkerRunning: api.workers.Load() > 0}
	raw, err := api.system.ReadMany(ctx, movieLikeKeys)
	if err != nil {
		return v, err
	}
	v.Basis = movielike.Hash([]string{raw["UserInstalledPlugins"], raw[movielike.ConfigKey], raw[movielike.ControlKey]})
	v.Fingerprint = api.fingerprint(raw, actor)
	v.Installed, err = indexerInstalled(raw["UserInstalledPlugins"], movielike.Plugin)
	if err != nil {
		return v, err
	}
	v.Spec, err = movielike.ParseSpec(raw[movielike.ConfigKey])
	if err != nil {
		return v, err
	}
	roots, rootErr := api.movieRoots(ctx)
	if rootErr == nil {
		v.Roots = roots
	}
	_, names, catErr := api.categories()
	if names != nil {
		v.Categories = names
	}
	_, clientErr := api.client()
	c, _, on, scopeErr := api.queue.Scope(ctx)
	v.Current = rootErr == nil && catErr == nil && clientErr == nil && scopeErr == nil
	v.Ready = v.Current && v.Installed && v.Spec.Enabled && v.Spec.Directory != "" && len(v.Roots) > 0
	v.Active = v.Current && on && api.validate(c) == nil
	if !v.Current {
		v.Reason = "原电影分类、Emby 或本机电影库范围不可用"
	} else if on && !v.Active {
		v.Reason = "原管理员、配置或媒体库目录身份已经变化"
	}
	return v, nil
}
func (api *movieLikeAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	base := "/api/v1/movie-like"
	if r.Method == "GET" && r.URL.Path == base {
		v, err := api.status(r.Context(), actor)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/review" {
		api.review(w, r, actor)
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/authorize" {
		api.activate(w, r, actor)
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/stop" {
		var in struct {
			Fingerprint string `json:"fingerprint"`
			Confirm     bool   `json:"confirm"`
		}
		if !decodeOrganizationRequest(w, r, &in, "无效停止确认") {
			return
		}
		v, err := api.status(r.Context(), actor)
		if err != nil || !in.Confirm || in.Fingerprint != v.Fingerprint {
			movieLikeError(w, movielike.ErrState)
			return
		}
		authority, err := api.authority(actor)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		validate := func() error {
			current, err := api.authority(actor)
			if err != nil || current != authority {
				return movielike.ErrState
			}
			return nil
		}
		if err := api.queue.Stop(r.Context(), v.Basis, validate); err != nil {
			movieLikeError(w, err)
			return
		}
		v, err = api.status(r.Context(), actor)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method == "GET" && r.URL.Path == base+"/receipts" {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeAPIError(w, 400, 400, "无效回执页码")
				return
			}
			page = n
		}
		rows, err := api.queue.List(r.Context(), page, false)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": rows})
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"/receipts/") {
		api.receiptAction(w, r, actor)
		return
	}
	writeAPIError(w, 404, 404, "未知电影精选操作")
}
func (api *movieLikeAPI) pluginPage(w http.ResponseWriter, r *http.Request) {
	rows, err := api.queue.List(r.Context(), 1, false)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	page := pluginPageData{Title: "电影精选原移动回执", ReadOnly: true, Sections: []string{"旧开关不自动授权移动。独立确认原分类和电影库完整范围后，只消费未来新回调。", "整部电影目录、字幕、封面、NFO 和其他附件一起移动；不覆盖目标，不新增或改写原整理历史。", "中断和进程重启不自动续跑，按同一原编号重新读取并独立确认继续。"}, Tables: []pluginPageTable{{Columns: []string{"原编号", "电影", "状态", "阶段"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}}}
	for _, p := range rows {
		page.Tables[0].Rows = append(page.Tables[0].Rows, []string{p.ID, p.Event.Name, p.State, p.Phase})
		page.Tables[0].RowActions = append(page.Tables[0].RowActions, nil)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": page})
}
func (api *movieLikeAPI) reservePlugin(next http.Handler) http.Handler {
	const base = "/api/v1/plugins/MovieLike"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			allowed := p == base && (r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") || p == base+"/install" && r.Method == "POST" || p == base+"/page" && r.Method == "GET"
			if !allowed {
				if _, ok := api.authorize(w, r); !ok {
					return
				}
				writeAPIError(w, 405, 405, "不支持此原生电影精选插件操作")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
