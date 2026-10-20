package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

const libraryPluginID = "LibraryScraper"
const libraryPluginKey = "plugin.LibraryScraper"
const libraryControlKey = "GoLibraryScraper"

var errLibraryPlugin = errors.New("invalid library scraper plugin configuration")
var libraryPluginKeys = []string{"UserInstalledPlugins", libraryPluginKey, libraryControlKey, scraper.ConfigKey}

type libraryPluginConfig struct {
	Cron, Mode           string
	Paths, IDs, Excludes []string
	Once                 bool
}
type libraryPluginLease struct {
	Actor, PluginDigest, Zone string
	Templates                 []organization.LibraryDefinition
}
type libraryPluginControl struct {
	Epoch                         string
	Authorized                    bool
	Lease                         libraryPluginLease
	State, Reason, Batch, Created string
	Next, OnceAt                  time.Time
	Jobs                          []string
	Empty                         int
}

func decodeLibraryControl(raw string) (libraryPluginControl, error) {
	c := libraryPluginControl{State: "unreviewed", Jobs: []string{}}
	if raw == "" {
		return c, nil
	}
	if len(raw) > 2<<20 || json.Unmarshal([]byte(raw), &c) != nil || len(c.Epoch) != 32 || len(c.Jobs) > 16 || len(c.Lease.Templates) > 16 {
		return c, errLibraryPlugin
	}
	switch c.State {
	case "stopped", "scheduled", "scanning", "queued", "paused", "idle":
	default:
		return c, errLibraryPlugin
	}
	return c, nil
}
func libraryPluginStrings(v any) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errLibraryPlugin
	}
	var out []string
	if json.Unmarshal(b, &out) != nil || len(out) > 128 {
		return nil, errLibraryPlugin
	}
	return out, nil
}
func parseLibraryPlugin(values map[string]any) (libraryPluginConfig, error) {
	c := libraryPluginConfig{Mode: "no_force"}
	for key, p := range map[string]*string{"cron": &c.Cron, "mode": &c.Mode} {
		if v, ok := values[key]; ok && v != nil {
			s, ok := v.(string)
			if !ok || len(s) > 512 {
				return c, errLibraryPlugin
			}
			*p = strings.TrimSpace(s)
		}
	}
	if !organization.LibraryMode(c.Mode) {
		return c, errLibraryPlugin
	}
	if v, ok := values["onlyonce"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return c, errLibraryPlugin
		}
		c.Once = b
	}
	var err error
	c.Paths, err = libraryPluginStrings(values["scraper_path"])
	if err != nil || len(c.Paths) > 16 {
		return c, errLibraryPlugin
	}
	c.IDs, err = libraryPluginStrings(values["go_root_ids"])
	if err != nil || len(c.IDs) > 0 && len(c.IDs) != len(c.Paths) {
		return c, errLibraryPlugin
	}
	for _, p := range c.Paths {
		if !filepath.IsAbs(p) || len(p) > 4096 || strings.ContainsAny(p, "\x00\r\n\\") {
			return c, errLibraryPlugin
		}
	}
	if v, ok := values["exclude_path"]; ok && v != nil {
		s, ok := v.(string)
		if !ok || len(s) > 64<<10 {
			return c, errLibraryPlugin
		}
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n\\") {
					return c, errLibraryPlugin
				}
				c.Excludes = append(c.Excludes, filepath.Clean(p))
			}
		}
	}
	if len(c.Excludes) > 128 || (c.Cron != "" || c.Once) && len(c.Paths) == 0 {
		return c, errLibraryPlugin
	}
	if c.Cron != "" {
		zone, err := rssWorkerLocation()
		if err != nil || len(strings.Fields(c.Cron)) != 5 {
			return c, errLibraryPlugin
		}
		s, err := parseRSSSchedule(c.Cron, zone)
		if err != nil || s.next(time.Now()).IsZero() {
			return c, errLibraryPlugin
		}
	}
	return c, nil
}
func (api *libraryScraper) pluginLease(ctx context.Context, values map[string]any, actor string) (libraryPluginLease, error) {
	l := libraryPluginLease{Actor: actor, PluginDigest: organization.Digest(values), Templates: []organization.LibraryDefinition{}}
	if !api.intake.organizer.auth.service.IsAdministrator(actor) {
		return l, errLibraryPlugin
	}
	snapshot, err := api.intake.organizer.service.configStore.Snapshot()
	if err != nil || text(objectValue(snapshot["app"])["login_user"]) != actor {
		return l, errLibraryPlugin
	}
	c, err := parseLibraryPlugin(values)
	if err != nil {
		return l, err
	}
	zone, err := rssWorkerLocation()
	if err != nil {
		return l, err
	}
	l.Zone = zone.String()
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		return l, err
	}
	seen := map[string]bool{}
	for i, p := range c.Paths {
		var found *organizationRoot
		for _, root := range roots.Targets {
			if root.Type != "MOV" && root.Type != "TV" && root.Type != "ANIME" {
				continue
			}
			if filepath.Clean(p) == root.Path && (len(c.IDs) == 0 || root.ID == c.IDs[i]) {
				v := root
				found = &v
				break
			}
		}
		if found == nil {
			return l, errLibraryPlugin
		}
		preview, err := api.definition(ctx, libraryPlanInput{RootID: found.ID, Path: ".", Mode: c.Mode, Excludes: []string{}, RequestID: strings.Repeat("0", 32)}, actor)
		if err != nil {
			return l, err
		}
		d := preview.definition
		d.Root.Kind = "auto"
		d.RequestID = ""
		if seen[d.Root.Identity] {
			return l, errLibraryPlugin
		}
		seen[d.Root.Identity] = true
		exclusions := map[string]bool{}
		for _, excluded := range c.Excludes {
			for _, base := range []string{found.Path, d.Root.Path} {
				parent, err := filepath.Rel(excluded, base)
				if err != nil {
					return l, err
				}
				if parent == "." || parent != ".." && !strings.HasPrefix(parent, ".."+string(filepath.Separator)) {
					exclusions["."] = true
					continue
				}
				rel, err := filepath.Rel(base, excluded)
				if err != nil {
					return l, err
				}
				if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					exclusions[rel] = true
				}
			}
		}
		for relative := range exclusions {
			d.Excludes = append(d.Excludes, relative)
		}
		sort.Strings(d.Excludes)
		l.Templates = append(l.Templates, d)
	}
	sort.Slice(l.Templates, func(i, j int) bool { return l.Templates[i].Root.ID < l.Templates[j].Root.ID })
	return l, nil
}
func (s pluginService) nativeLibrary(id string) bool {
	return s.library != nil && strings.TrimSpace(id) == libraryPluginID
}
func libraryControlJSON(c libraryPluginControl) (string, error) {
	b, err := json.Marshal(c)
	if err != nil || len(b) > 2<<20 {
		return "", errLibraryPlugin
	}
	return string(b), nil
}
func (api *libraryScraper) libraryDetail(ctx context.Context, values map[string]any, raw map[string]string) (pluginConfigDetail, error) {
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	choices := []pluginConfigChoice{}
	selected := []string{}
	paths, _ := libraryPluginStrings(values["scraper_path"])
	ids, _ := libraryPluginStrings(values["go_root_ids"])
	for _, r := range roots.Targets {
		if r.Type != "MOV" && r.Type != "TV" && r.Type != "ANIME" {
			continue
		}
		choices = append(choices, pluginConfigChoice{r.ID, r.Label + " · " + r.Path})
	}
	for i, p := range paths {
		for _, r := range roots.Targets {
			if (r.Type == "MOV" || r.Type == "TV" || r.Type == "ANIME") && filepath.Clean(p) == r.Path && (len(ids) == 0 || i < len(ids) && ids[i] == r.ID) {
				selected = append(selected, r.ID)
				break
			}
		}
	}
	fields := []pluginConfigField{
		{Key: "cron", Title: "刮削周期（五段 cron）", Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Tooltip: "留空保留，明确清除则停用周期；星期一为 0，日期与星期同时匹配。"},
		{Key: "mode", Title: "刮削模式", Type: "select", Options: []pluginConfigChoice{{"no_force", "仅补缺失"}, {"force_nfo", "仅覆盖所选 NFO"}, {"force_all", "覆盖所选 NFO 与图片"}}},
		{Key: "scraper_path", Title: "媒体库", Type: "multiselect", Options: choices, Tooltip: "每轮重新扫描这些根，自动识别电影／剧集；单轮最多 50,000 个媒体。"},
		{Key: "exclude_path", Title: "排除的绝对路径（逗号分隔）", Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Tooltip: "按完整路径边界排除，不匹配表达式。"},
		{Key: "onlyonce", Title: "仅运行一次（最早三秒后）", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "与周期独立，登记时消费此开关；后台每 15 秒检查，忙时顺延，重启不重复登记。"},
		{Key: "go_stop", Title: "只停止周期及本插件后续写入", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "保留原配置、文件、备份与不确定占用，不影响独立手动任务。"},
		{Key: "go_confirm", Title: "确认本次所选范围的后台写入授权", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_old_stopped", Title: "确认旧 Python、其他实例及外部写入器已实际停止", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_overwrite", Title: "独立同意覆盖所选元数据并保留额外磁盘备份", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "两种强制模式必须确认；备份不会自动删除。"},
	}
	for i := range fields {
		fields[i].Configured = configuredPluginValue(values[fields[i].Key])
	}
	mode := "no_force"
	if v, ok := values["mode"].(string); ok {
		mode = v
	}
	return pluginConfigDetail{ID: libraryPluginID, Name: "媒体库刮削", Fields: fields, Values: map[string]any{"mode": mode, "scraper_path": selected, "onlyonce": false, "go_stop": false, "go_confirm": false, "go_old_stopped": false, "go_overwrite": false}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: organization.Digest(raw)}}, nil
}
func (s pluginService) serveLibraryPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.library
	if api == nil || !api.authorize(w, r) {
		return
	}
	claims, err := s.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, err := s.system.ReadMany(ctx, libraryPluginKeys)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], libraryPluginID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	values, configErr := decodeMetadataConfig(raw[libraryPluginKey])
	control, controlErr := decodeLibraryControl(raw[libraryControlKey])
	write := action == "install" || action == "uninstall" || action == "save" || action == "legacy-save"
	if !write {
		if configErr != nil || controlErr != nil {
			writeAPIError(w, 503, 503, "library scraper plugin state unavailable")
			return
		}
		if action == "status" {
			running, known := s.librarySummary(ctx)
			var state any
			if installed && known {
				state = running
			}
			writeJSON(w, 200, map[string]any{"code": 0, "state": state, "stateKnown": known})
			return
		}
		if !installed {
			writeAPIError(w, 404, 404, "plugin not installed")
			return
		}
		if action == "page" {
			api.writePluginPage(w, r, values, control)
			return
		}
		detail, err := api.libraryDetail(ctx, values, raw)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": detail})
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		if action == "uninstall" && r.URL.Path == "/api/v1/plugin/uninstall" {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true})
			return
		}
		writeAPIError(w, 409, 409, "plugin not installed")
		return
	}
	if controlErr != nil && action != "uninstall" {
		writeAPIError(w, 409, 409, "stored schedule state requires inspection")
		return
	}
	next := values
	var input pluginConfigRequest
	if action == "save" {
		if !decodeServiceRequest(w, r, &input, "invalid library scraper settings") {
			return
		}
		if input.Fingerprint != organization.Digest(raw) {
			writeAPIError(w, 409, 409, "plugin settings changed; reload before retrying")
			return
		}
		if configErr != nil {
			writeAPIError(w, 409, 409, "stored configuration invalid; use a complete legacy replacement")
			return
		}
		detail, err := api.libraryDetail(ctx, values, raw)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		var message string
		next, message = mergePluginConfig(values, detail.Fields, input)
		if message != "" {
			writeAPIError(w, 400, 400, "invalid library scraper fields")
			return
		}
		selected, err := libraryPluginStrings(next["scraper_path"])
		if err != nil {
			writeAPIError(w, 400, 400, "invalid library root selection")
			return
		}
		roots, err := api.intake.organizer.roots(ctx)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		paths := []string{}
		for _, id := range selected {
			found := false
			for _, root := range roots.Targets {
				if root.ID == id && (root.Type == "MOV" || root.Type == "TV" || root.Type == "ANIME") {
					paths = append(paths, root.Path)
					found = true
					break
				}
			}
			if !found {
				writeAPIError(w, 400, 400, "library root no longer configured")
				return
			}
		}
		next["scraper_path"], next["go_root_ids"] = paths, selected
	}
	if action == "legacy-save" {
		next, err = decodeMetadataConfig(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "invalid library scraper configuration")
			return
		}
		selected, err := libraryPluginStrings(next["scraper_path"])
		if err != nil {
			writeAPIError(w, 400, 400, "invalid library selection")
			return
		}
		needsMap := false
		for _, p := range selected {
			if !filepath.IsAbs(p) {
				needsMap = true
			}
		}
		if needsMap {
			roots, err := api.intake.organizer.roots(ctx)
			if err != nil {
				writeOrganizationError(w, err)
				return
			}
			paths, ids := []string{}, []string{}
			for _, p := range selected {
				found := false
				for _, root := range roots.Targets {
					if (root.Type == "MOV" || root.Type == "TV" || root.Type == "ANIME") && (root.ID == p || filepath.IsAbs(p) && root.Path == filepath.Clean(p)) {
						paths = append(paths, root.Path)
						ids = append(ids, root.ID)
						found = true
						break
					}
				}
				if !found {
					writeAPIError(w, 400, 400, "library no longer configured")
					return
				}
			}
			next["scraper_path"], next["go_root_ids"] = paths, ids
		}
	}
	if next == nil {
		next = map[string]any{}
	}
	// Unknown persisted cache keys can never be carried forward as consent.
	stop, activate, oldStopped, overwrite := false, false, false, false
	if action == "save" {
		stop = input.Values["go_stop"] == true
		activate = input.Values["go_confirm"] == true
		oldStopped = input.Values["go_old_stopped"] == true
		overwrite = input.Values["go_overwrite"] == true
	}
	if action == "legacy-save" {
		stop = next["go_stop"] == true
		activate = r.PostForm.Get("confirm") == "true"
		oldStopped = r.PostForm.Get("confirmOldExecutorsStopped") == "true"
		overwrite = r.PostForm.Get("confirmOverwrite") == "true"
	}
	for _, k := range []string{"go_stop", "go_confirm", "go_old_stopped", "go_overwrite"} {
		delete(next, k)
	}
	epoch, err := newIndexerClaim()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	state := libraryPluginControl{Epoch: epoch, State: "stopped", Reason: "native scheduling requires explicit reviewed authorization", Jobs: []string{}}
	changes := map[string]string{}
	if action == "save" || action == "legacy-save" {
		cfg, err := parseLibraryPlugin(next)
		if err != nil && !stop {
			writeAPIError(w, 400, 400, "invalid cron, mode, selected roots or exclusions")
			return
		}
		if !stop && (cfg.Cron != "" || cfg.Once) {
			if !activate || !oldStopped || cfg.Mode != "no_force" && !overwrite {
				writeAPIError(w, 400, 400, "explicit recurring/once authorization, stopped writers and independent overwrite consent required")
				return
			}
			if action == "legacy-save" && r.PostForm.Get("fingerprint") != organization.Digest(raw) {
				writeAPIError(w, 409, 409, "fresh plugin configuration fingerprint required; do not replay an uncertain once request")
				return
			}
			next["onlyonce"] = false
			lease, err := api.pluginLease(ctx, next, claims.Username)
			if err != nil {
				writeAPIError(w, 409, 409, "selected library scope changed or unavailable")
				return
			}
			state.Authorized, state.Lease, state.State, state.Reason = true, lease, "scheduled", ""
			if cfg.Once {
				state.OnceAt = time.Now().Add(3 * time.Second).UTC()
			}
			if cfg.Cron != "" {
				zone, _ := rssWorkerLocation()
				schedule, _ := parseRSSSchedule(cfg.Cron, zone)
				state.Next = schedule.next(time.Now()).UTC()
			}
		} else {
			next["onlyonce"] = false
			state.Reason = "explicitly stopped; files, backups and uncertain receipts retained"
		}
		b, err := json.Marshal(next)
		if err != nil || len(b) > 64<<10 {
			writeAPIError(w, 400, 400, "invalid library scraper settings")
			return
		}
		changes[libraryPluginKey] = string(b)
	}
	ids, err := decodeInstalledPlugins(raw["UserInstalledPlugins"])
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	out := []string{}
	for _, id := range ids {
		if id != libraryPluginID || action != "uninstall" {
			out = append(out, id)
		}
	}
	if action == "install" {
		if len(out) >= 1024 {
			writeAPIError(w, 400, 400, "too many installed plugins")
			return
		}
		out = append(out, libraryPluginID)
	}
	b, _ := json.Marshal(out)
	changes["UserInstalledPlugins"] = string(b)
	encoded, err := libraryControlJSON(state)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	changes[libraryControlKey] = encoded
	var run *organization.LibraryPluginRun
	if controlErr == nil && control.Batch != "" && (control.State == "scanning" || control.State == "queued") {
		run = &organization.LibraryPluginRun{ID: control.Batch, Epoch: control.Epoch, State: "revoked", Reason: "plugin authorization changed; prior effects and uncertain claims retained", Created: control.Created, Updated: time.Now().UTC().Format(time.RFC3339Nano), Jobs: control.Jobs}
	}
	revoke := control.Epoch
	if action == "uninstall" {
		revoke = "*"
	}
	err = api.intake.organizer.jobs.LibraryPluginMutation(ctx, raw, changes, revoke, run, nil)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "msg": "操作成功", "message": "设置与调度意图已保存；未重放任何旧刮削。请到插件回执及整理页核查结果。"})
}
func (s pluginService) librarySummary(ctx context.Context) (bool, bool) {
	if s.library == nil || !s.library.intake.organizer.pureGo {
		return false, false
	}
	raw, err := s.system.ReadMany(ctx, libraryPluginKeys)
	if err != nil {
		return false, false
	}
	c, err := decodeLibraryControl(raw[libraryControlKey])
	if err != nil {
		return false, false
	}
	if !c.Authorized {
		return false, true
	}
	values, err := decodeMetadataConfig(raw[libraryPluginKey])
	if err != nil {
		return false, false
	}
	lease, err := s.library.pluginLease(ctx, values, c.Lease.Actor)
	if err != nil || organization.Digest(lease) != organization.Digest(c.Lease) {
		return false, false
	}
	active := !c.Next.IsZero() || !c.OnceAt.IsZero() || c.State == "queued" || c.State == "scanning"
	return c.State != "paused" && active && s.library.workers.Load() > 0, true
}
func (api *libraryScraper) writePluginPage(w http.ResponseWriter, r *http.Request, values map[string]any, c libraryPluginControl) {
	runs, err := api.intake.organizer.jobs.LibraryPluginRuns(r.Context())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	data := pluginPageData{Title: "媒体库刮削调度与批次", ReadOnly: true, Sections: []string{"只读查看，不触发扫描、刮削或恢复。逐文件进度、备份恢复与不确定结果核对请到整理页。", "已有配置升级后不会自动接管；配置／选项／来源授权变化会暂停旧调度，需重新明确确认。", "状态：" + c.State, "原因：" + c.Reason}, Tables: []pluginPageTable{}}
	if api.workers.Load() == 0 {
		data.Sections = append(data.Sections, "原生后台执行器尚未启动；handler-only 模式不会自动执行任务。")
	}
	if !c.OnceAt.IsZero() {
		data.Sections = append(data.Sections, "仅运行一次的登记时间："+c.OnceAt.Format(time.RFC3339))
	}
	if !c.Next.IsZero() {
		data.Sections = append(data.Sections, "下次周期："+c.Next.Format(time.RFC3339))
	}
	table := pluginPageTable{Columns: []string{"批次", "状态", "任务编号", "空媒体库数", "说明"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, run := range runs {
		table.Rows = append(table.Rows, []string{run.ID, run.State, strings.Join(run.Jobs, ", "), text(run.Empty), run.Reason})
		table.RowActions = append(table.RowActions, nil)
	}
	data.Tables = append(data.Tables, table)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
