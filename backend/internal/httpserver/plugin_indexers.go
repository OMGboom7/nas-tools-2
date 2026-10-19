package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

var errIndexerPlugin = errors.New("invalid indexer plugin state or configuration")
var errIndexerScope = errors.New("indexer plugin scope changed")

type nativeIndexerPlugins struct {
	system    *systemconfig.Store
	transport http.RoundTripper
	pureGo    bool
}
type indexerPluginRow struct {
	ID, Name, Domain string
	Public           bool
}
type indexerPluginState struct {
	ConfigDigest           string
	Claim                  string
	Status                 string
	Started, Checked, Next time.Time
	Items                  []indexerPluginRow
}

func nativeIndexerPlugin(id string) bool { return id == "Jackett" || id == "Prowlarr" }
func (s pluginService) nativeIndexer(id string) bool {
	return s.indexers != nil && nativeIndexerPlugin(strings.TrimSpace(id))
}
func indexerKeys(id string) []string {
	return []string{"UserInstalledPlugins", "plugin." + id, "GoIndexerPlugin." + id}
}
func newIndexerClaim() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func encodeIndexerState(s indexerPluginState) (string, error) {
	b, err := json.Marshal(s)
	if err != nil || len(b) > 1<<20 {
		return "", errIndexerPlugin
	}
	return string(b), nil
}
func decodeIndexerState(raw string) (indexerPluginState, error) {
	var s indexerPluginState
	if raw == "" {
		return s, nil
	}
	if strings.TrimSpace(raw) == "null" || len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &s) != nil || len(s.Items) > 1024 {
		return s, errIndexerPlugin
	}
	switch s.Status {
	case "", "idle", "checking", "ready", "failed":
	default:
		return s, errIndexerPlugin
	}
	return s, nil
}
func indexerInstalled(raw, id string) (bool, error) {
	ids, err := decodeInstalledPlugins(raw)
	if err != nil {
		return false, err
	}
	for _, x := range ids {
		if x == id {
			return true, nil
		}
	}
	return false, nil
}

func indexerConfiguration(id string, values map[string]any) (externalindexer.Config, bool, error) {
	c := externalindexer.Config{Kind: id}
	if !nativeIndexerPlugin(id) {
		return c, false, errIndexerPlugin
	}
	for key, target := range map[string]*string{"host": &c.Host, "api_key": &c.APIKey, "password": &c.Password} {
		if raw, ok := values[key]; ok {
			v, valid := raw.(string)
			if !valid {
				return c, false, errIndexerPlugin
			}
			*target = v
		}
	}
	for _, key := range []string{"onlyonce", "refresh_enabled"} {
		if raw, ok := values[key]; ok {
			if _, valid := raw.(bool); !valid {
				return c, false, errIndexerPlugin
			}
		}
	}
	cron := "0 0 */24 * *" // Preserve the legacy five-field default, not a 24h interval.
	if raw, ok := values["cron"]; ok {
		v, valid := raw.(string)
		if !valid {
			return c, false, errIndexerPlugin
		}
		if strings.TrimSpace(v) != "" {
			cron = v
		}
	}
	if len(strings.Fields(cron)) != 5 {
		return c, false, errIndexerPlugin
	}
	zone, err := rssWorkerLocation()
	if err != nil {
		return c, false, errIndexerPlugin
	}
	if _, err := parseRSSSchedule(cron, zone); err != nil {
		return c, false, errIndexerPlugin
	}
	if c.Host == "" && c.APIKey == "" {
		return c, false, nil
	}
	if externalindexer.Validate(c) != nil {
		return c, false, errIndexerPlugin
	}
	return c, true, nil
}

func indexerDetail(id string, values map[string]any) pluginConfigDetail {
	fields := []pluginConfigField{
		{Key: "host", Title: id + " 地址", Type: "text", Required: true, WriteOnly: true, Options: []pluginConfigChoice{}, Placeholder: "http://127.0.0.1:9696"},
		{Key: "api_key", Title: "API Key", Type: "text", Required: true, WriteOnly: true, Options: []pluginConfigChoice{}},
		{Key: "cron", Title: "更新周期（五段 cron）", Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Placeholder: "0 0 */24 * *", Tooltip: "按 APScheduler 规则：星期一为 0，日与星期同时匹配；留空保留原值。旧默认表达式不是每隔 24 小时。"},
		{Key: "refresh_enabled", Title: "启用周期读取索引器列表", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "只读取列表，不搜索、不下种；关闭不影响实际检索时的实时发现。"},
		{Key: "onlyonce", Title: "立即读取一次，并启用周期", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "纯 Go 后台消费一次后自动关闭此开关，保留周期刷新。"},
	}
	if id == "Jackett" {
		fields[0].Placeholder = "http://127.0.0.1:9117"
		fields = append(fields, pluginConfigField{Key: "password", Title: "管理密码（可选）", Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}})
	}
	for i := range fields {
		fields[i].Configured = configuredPluginValue(values[fields[i].Key])
	}
	return pluginConfigDetail{ID: id, Name: id, Fields: fields, Values: map[string]any{"refresh_enabled": values["refresh_enabled"] == true, "onlyonce": values["onlyonce"] == true}, Meta: pluginConfigDetailMeta{HasPage: true}}
}

func (m *nativeIndexerPlugins) read(ctx context.Context, id string) (map[string]any, indexerPluginState, bool, error) {
	k := indexerKeys(id)
	raw, err := m.system.ReadMany(ctx, k)
	if err != nil {
		return nil, indexerPluginState{}, false, err
	}
	installed, err := indexerInstalled(raw[k[0]], id)
	if err != nil {
		return nil, indexerPluginState{}, false, err
	}
	values, err := decodeMetadataConfig(raw[k[1]])
	if err != nil {
		return nil, indexerPluginState{}, installed, err
	}
	state, err := decodeIndexerState(raw[k[2]])
	if err == nil && state.ConfigDigest != bus.Hash(values) {
		state = indexerPluginState{}
	}
	return values, state, installed, err
}

func (m *nativeIndexerPlugins) summary(ctx context.Context, id string) (bool, bool) {
	values, state, installed, err := m.read(ctx, id)
	if err != nil || !installed {
		return false, false
	}
	_, configured, err := indexerConfiguration(id, values)
	if err != nil {
		return false, false
	}
	if !configured {
		return false, true
	}
	now := time.Now()
	known := state.Status == "ready" && !state.Checked.IsZero() && !now.Before(state.Checked) && now.Before(state.Checked.Add(30*time.Minute))
	return known && len(state.Items) > 0, known
}

// All native mutation paths are admin-only and pure-Go. Saving and uninstalling
// invalidate an in-flight publication in the same transaction, without waiting
// on the remote service. Configuration never becomes a fictitious probe result.
func (s pluginService) serveIndexerPlugin(w http.ResponseWriter, r *http.Request, action string) {
	if s.auth == nil || s.indexers == nil {
		writeAPIError(w, 503, 503, "native indexer plugin service unavailable")
		return
	}
	claims, err := s.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	if _, err := s.auth.service.FindUser(r.Context(), claims.Username); err != nil {
		writeAPIError(w, 401, 401, "account unavailable")
		return
	}
	write := action == "install" || action == "uninstall" || action == "save" || action == "legacy-save"
	if write && !s.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission required")
		return
	}
	if write && !s.indexers.pureGo {
		writeAPIError(w, 501, 501, "native indexer plugin changes require disabled legacy backend")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !nativeIndexerPlugin(id) {
		writeAPIError(w, 404, 404, "indexer plugin not found")
		return
	}
	if !write {
		values, state, installed, err := s.indexers.read(r.Context(), id)
		if err != nil {
			writeAPIError(w, 502, 502, "indexer plugin state unavailable")
			return
		}
		if action == "status" {
			running, known := s.indexers.summary(r.Context(), id)
			var value any
			if installed && known {
				value = running
			}
			writeJSON(w, 200, map[string]any{"code": 0, "state": value, "stateKnown": installed && known})
			return
		}
		if !installed {
			writeAPIError(w, 404, 404, "plugin is not installed")
			return
		}
		if action == "page" {
			s.writeIndexerPage(w, id, values, state)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": indexerDetail(id, values)})
		return
	}
	var input pluginConfigRequest
	var complete map[string]any
	if action == "save" {
		if !decodeServiceRequest(w, r, &input, "invalid indexer configuration") {
			return
		}
	}
	if action == "legacy-save" {
		raw := r.PostForm.Get("config")
		var err error
		complete, err = decodeMetadataConfig(raw)
		if err != nil || raw == "" {
			writeAPIError(w, 400, 400, "invalid indexer configuration")
			return
		}
	}
	k := indexerKeys(id)
	err = s.system.UpdateMany(r.Context(), k, func(raw map[string]string) (map[string]string, error) {
		ids, err := decodeInstalledPlugins(raw[k[0]])
		if err != nil {
			return nil, err
		}
		installed := false
		next := []string{}
		for _, x := range ids {
			if x == id {
				installed = true
				if action == "uninstall" {
					continue
				}
			}
			next = append(next, x)
		}
		if action == "uninstall" && !installed {
			return nil, errPluginNotInstalled
		}
		if action == "install" {
			if installed {
				return nil, nil
			}
			if len(next) >= 1024 {
				return nil, errIndexerPlugin
			}
			next = append(next, id)
		} else if !installed {
			return nil, errPluginNotInstalled
		}
		current, err := decodeMetadataConfig(raw[k[1]])
		if err != nil && action != "legacy-save" && action != "uninstall" {
			return nil, err
		}
		if complete != nil {
			current = complete
		}
		if action == "save" {
			var message string
			current, message = mergePluginConfig(current, indexerDetail(id, current).Fields, input)
			if message != "" {
				return nil, errIndexerPlugin
			}
		}
		result := map[string]string{}
		if action != "uninstall" {
			_, configured, err := indexerConfiguration(id, current)
			if err != nil || (action == "save" || action == "legacy-save") && !configured {
				return nil, errIndexerPlugin
			}
			if current["onlyonce"] == true {
				current["refresh_enabled"] = true
			}
			if action == "save" || action == "legacy-save" {
				b, err := json.Marshal(current)
				if err != nil || len(b) > 64<<10 {
					return nil, errIndexerPlugin
				}
				result[k[1]] = string(b)
			}
		}
		encoded, _ := json.Marshal(next)
		result[k[0]] = string(encoded)
		// A new claim invalidates an old read, including uninstall/reinstall ABA.
		claim, err := newIndexerClaim()
		if err != nil {
			return nil, err
		}
		state, err := encodeIndexerState(indexerPluginState{Claim: claim, Status: "idle"})
		if err != nil {
			return nil, err
		}
		result[k[2]] = state
		return result, nil
	})
	if err != nil {
		if errors.Is(err, errPluginNotInstalled) && action == "uninstall" && r.URL.Path == "/api/v1/plugin/uninstall" {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "msg": "插件已卸载"})
			return
		}
		status := 502
		if errors.Is(err, errPluginNotInstalled) {
			status = 409
		}
		if errors.Is(err, errIndexerPlugin) {
			status = 400
		}
		writeAPIError(w, status, status, "indexer plugin operation rejected; configuration and state were not changed")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "索引器插件设置已保存；后台只读取列表，不搜索或下种", "msg": "操作成功"})
}

func (s pluginService) writeIndexerPage(w http.ResponseWriter, id string, values map[string]any, state indexerPluginState) {
	data := pluginPageData{Title: id + " 索引列表", ReadOnly: true, Sections: []string{}, Tables: []pluginPageTable{}, DeleteConfirmation: ""}
	if !s.indexers.pureGo {
		data.Sections = append(data.Sections, "当前非纯 Go 模式，原生周期读取不会启动。")
	}
	data.Sections = append(data.Sections, "此页为带时间的列表快照，不代表当前检索成功；实际检索仍实时读取索引器。")
	if state.Checked.IsZero() {
		data.Sections = append(data.Sections, "尚无当前配置的成功读取记录。")
	} else {
		data.Sections = append(data.Sections, "最近读取："+state.Checked.Format(time.RFC3339))
	}
	if state.Status == "checking" {
		data.Sections = append(data.Sections, "正在读取索引器列表。")
	}
	if state.Status == "failed" {
		data.Sections = append(data.Sections, "最近读取失败，保留的旧快照不能证明当前可用；请检查服务、配置或验证状态。")
	}
	if values["onlyonce"] == true {
		data.Sections = append(data.Sections, "已登记一次读取，等待纯 Go 后台消费。")
	}
	if !state.Next.IsZero() {
		data.Sections = append(data.Sections, "下次周期读取："+state.Next.Format(time.RFC3339))
	}
	table := pluginPageTable{Columns: []string{"ID", "索引名称", "服务域名", "是否公开"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, item := range state.Items {
		public := "否"
		if item.Public {
			public = "是"
		}
		table.Rows = append(table.Rows, []string{cleanPluginPageText(item.ID, 256), cleanPluginPageText(item.Name, 256), item.Domain, public})
		table.RowActions = append(table.RowActions, nil)
	}
	data.Tables = append(data.Tables, table)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}

func (m *nativeIndexerPlugins) tick(ctx context.Context, now time.Time) error {
	if m == nil || !m.pureGo {
		return nil
	}
	var first error
	for _, id := range []string{"Jackett", "Prowlarr"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.refresh(ctx, id, now); err != nil && !errors.Is(err, errIndexerScope) && first == nil {
			first = err
		}
	}
	return first
}

func (m *nativeIndexerPlugins) refresh(ctx context.Context, id string, now time.Time) error {
	if m == nil || !m.pureGo {
		return nil
	}
	k := indexerKeys(id)
	var config externalindexer.Config
	var scope indexerPluginState
	claimed := false
	err := m.system.UpdateMany(ctx, k, func(raw map[string]string) (map[string]string, error) {
		installed, err := indexerInstalled(raw[k[0]], id)
		if err != nil || !installed {
			return nil, err
		}
		values, err := decodeMetadataConfig(raw[k[1]])
		if err != nil {
			return nil, err
		}
		var configured bool
		config, configured, err = indexerConfiguration(id, values)
		if err != nil || !configured {
			return nil, err
		}
		state, err := decodeIndexerState(raw[k[2]])
		if err != nil {
			return nil, err
		}
		digest := bus.Hash(values)
		changed := state.ConfigDigest != digest
		if !changed && state.Status == "checking" && now.Before(state.Started.Add(time.Minute)) {
			return nil, nil
		}
		due := values["refresh_enabled"] == true && !state.Next.IsZero() && !now.Before(state.Next)
		interrupted := !changed && state.Status == "checking" && !now.Before(state.Started.Add(time.Minute))
		if !changed && values["onlyonce"] != true && !due && !interrupted {
			return nil, nil
		}
		if changed {
			state = indexerPluginState{}
		}
		result := map[string]string{}
		if values["onlyonce"] == true {
			values["onlyonce"] = false
			values["refresh_enabled"] = true
			b, err := json.Marshal(values)
			if err != nil || len(b) > 64<<10 {
				return nil, errIndexerPlugin
			}
			result[k[1]] = string(b)
			digest = bus.Hash(values)
		}
		state.ConfigDigest = digest
		state.Status = "checking"
		state.Started = now.UTC()
		state.Next = time.Time{}
		if values["refresh_enabled"] == true {
			cron := text(values["cron"])
			if strings.TrimSpace(cron) == "" {
				cron = "0 0 */24 * *"
			}
			zone, err := rssWorkerLocation()
			if err != nil {
				return nil, err
			}
			schedule, err := parseRSSSchedule(cron, zone)
			if err != nil {
				return nil, err
			}
			state.Next = schedule.next(now).UTC()
			if state.Next.IsZero() {
				return nil, errIndexerPlugin
			}
		}
		state.Claim, err = newIndexerClaim()
		if err != nil {
			return nil, err
		}
		encoded, err := encodeIndexerState(state)
		if err != nil {
			return nil, err
		}
		result[k[2]] = encoded
		scope = state
		claimed = true
		return result, nil
	})
	if err != nil || !claimed {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	items, probeErr := externalindexer.Discover(call, config, m.transport)
	cancel()
	persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	return m.system.UpdateMany(persist, k, func(raw map[string]string) (map[string]string, error) {
		installed, err := indexerInstalled(raw[k[0]], id)
		if err != nil || !installed {
			return nil, errIndexerScope
		}
		values, err := decodeMetadataConfig(raw[k[1]])
		if err != nil || bus.Hash(values) != scope.ConfigDigest {
			return nil, errIndexerScope
		}
		state, err := decodeIndexerState(raw[k[2]])
		if err != nil || state.Claim != scope.Claim || state.Status != "checking" {
			return nil, errIndexerScope
		}
		state.Status = "failed"
		state.Checked = now.UTC()
		if probeErr == nil {
			state.Status = "ready"
			state.Items = []indexerPluginRow{}
			for _, item := range items {
				state.Items = append(state.Items, indexerPluginRow{ID: item.ID, Name: item.Name, Domain: indexercatalog.Domain(item.Domain), Public: item.Public})
			}
		}
		encoded, err := encodeIndexerState(state)
		if err != nil {
			return nil, err
		}
		return map[string]string{k[2]: encoded}, nil
	})
}

func startIndexerPluginWorker(ctx context.Context, m *nativeIndexerPlugins) func() {
	if m == nil || !m.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		failed := false
		for {
			if ctx.Err() != nil {
				return
			}
			err := m.tick(ctx, time.Now())
			if err != nil && !failed && ctx.Err() == nil {
				slog.Warn("native indexer plugin list refresh unavailable; inspect plugin configuration and snapshot")
			}
			failed = err != nil
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}
