package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type iyuuBinding struct {
	Fingerprint string
	RequestID   string
	SID         string
	Site        string
	UID         string
	Passkey     string
}

type iyuuReceipt struct {
	ID    string `json:"id"`
	Site  string `json:"site"`
	State string `json:"state"`
}

func (api *iyuuAPI) requestScope(w http.ResponseWriter, r *http.Request, needsSeedRuntime bool) (iyuuScope, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, ok := api.transfer.login.actor(w, r, false)
	if !ok {
		return iyuuScope{}, false
	}
	if needsSeedRuntime && !api.transfer.login.pureGo {
		writeAPIError(w, 501, 501, "种子匹配和辅种任务需要 Go 独立运行模式")
		return iyuuScope{}, false
	}
	s, err := api.scope(r.Context(), actor)
	if err != nil {
		writeAPIError(w, 409, 409, "请确认已安装 IYUU 插件并保存有效的 Token")
		return s, false
	}
	return s, true
}

// The local request ID remains consumed even if the process exits between
// sending the remote binding and recording its response.
func (api *iyuuAPI) bindingTx(ctx context.Context, s iyuuScope, apply func(*sql.Tx) error) error {
	return api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_IYUU_BINDINGS (ID TEXT PRIMARY KEY, ACCOUNT TEXT NOT NULL, INTENT TEXT NOT NULL, SITE TEXT NOT NULL, STATE TEXT NOT NULL)`); err != nil {
			return err
		}
		return systemconfig.UpdateManyTx(ctx, tx, iyuuKeys, func(raw map[string]string) (map[string]string, error) {
			if !maps.Equal(s.raw, raw) {
				return nil, iyuu.ErrInput
			}
			return nil, apply(tx)
		})
	})
}

func (s iyuuScope) account() string { return sitelogin.Hash([]string{s.actor, s.cfg.Token}) }

func (api *iyuuAPI) overview(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	var latest iyuuReceipt
	err := api.bindingTx(ctx, s, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT ID,SITE,STATE FROM GO_IYUU_BINDINGS WHERE ACCOUNT=? ORDER BY rowid DESC LIMIT 1", s.account()).Scan(&latest.ID, &latest.Site, &latest.State)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	id, nonceErr := refreshNonce()
	if err != nil || nonceErr != nil {
		writeAPIError(w, 503, 503, "读取 IYUU 绑定记录失败")
		return
	}
	if !api.transfer.login.pureGo {
		signinJSON(w, map[string]any{"fingerprint": s.proof, "requestId": id, "latest": latest, "matchFingerprint": "", "downloaders": []string{}})
		return
	}
	selection, err := api.selection(ctx, s)
	if err != nil {
		writeAPIError(w, 409, 409, "读取下载器或站点设置失败，请核对配置")
		return
	}
	st, err := readIyuuSchedule(s.raw[iyuuControlKey])
	if err != nil {
		writeAPIError(w, 503, 503, "读取自动辅种记录失败")
		return
	}
	schedule := map[string]string{"last": st.Last}
	if !st.Next.IsZero() {
		schedule["next"] = st.Next.Format(time.RFC3339)
	}
	if !st.Once.IsZero() {
		schedule["once"] = st.Once.Format(time.RFC3339)
	}
	signinJSON(w, map[string]any{"schedule": schedule, "fingerprint": s.proof, "requestId": id, "latest": latest, "matchFingerprint": selection.proof, "downloaders": selection.names})
}

func (api *iyuuAPI) partners(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r, false)
	if !ok {
		return
	}
	partners, ok := api.checkedPartners(w, r, s)
	if !ok {
		return
	}
	rows := make([]map[string]string, 0, len(partners))
	for _, p := range partners {
		rows = append(rows, map[string]string{"sid": p.ID.String(), "site": p.Site, "name": p.Nickname})
	}
	signinJSON(w, map[string]any{"fingerprint": s.proof, "items": rows})
}

func (api *iyuuAPI) checkedPartners(w http.ResponseWriter, r *http.Request, s iyuuScope) ([]iyuu.Partner, bool) {
	partners, err := s.client.Partners(r.Context())
	if err != nil {
		writeAPIError(w, 502, 502, "读取 IYUU 合作站点失败")
		return nil, false
	}
	if api.current(r.Context(), s) != nil {
		writeAPIError(w, 409, 409, "IYUU 设置已变化，请刷新")
		return nil, false
	}
	return partners, true
}

func (api *iyuuAPI) bind(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r, false)
	if !ok {
		return
	}
	var in iyuuBinding
	if !siteLoginJSON(w, r, &in) {
		return
	}
	result, err := api.bindSite(r.Context(), s, in)
	if err != nil {
		if errors.Is(err, iyuu.ErrInput) {
			writeAPIError(w, 409, 409, "请刷新站点列表并核对绑定资料")
		} else {
			writeAPIError(w, 502, 502, "无法核实 IYUU 绑定，请重新打开页面查看回执")
		}
		return
	}
	signinJSON(w, result)
}

func (api *iyuuAPI) bindSite(ctx context.Context, s iyuuScope, in iyuuBinding) (iyuuReceipt, error) {
	var result iyuuReceipt
	if in.Fingerprint != s.proof || !refreshID(in.RequestID, 32) || !iyuu.ValidBinding(in.UID, in.Passkey) {
		return result, iyuu.ErrInput
	}
	intent := sitelogin.Hash([]string{s.account(), in.Site, in.SID, in.UID, in.Passkey})
	result = iyuuReceipt{ID: in.RequestID, Site: in.Site, State: "pending"}
	read := func(tx *sql.Tx) error {
		var saved string
		err := tx.QueryRowContext(ctx, "SELECT INTENT,STATE FROM GO_IYUU_BINDINGS WHERE ID=?", in.RequestID).Scan(&saved, &result.State)
		if err == nil && saved != intent {
			return iyuu.ErrInput
		}
		return err
	}
	err := api.bindingTx(ctx, s, read)
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	partners, err := s.client.Partners(ctx)
	if err != nil {
		return result, err
	}
	var partner iyuu.Partner
	for _, p := range partners {
		if p.ID.String() == in.SID && p.Site == in.Site {
			partner = p
			break
		}
	}
	if partner.Site == "" || api.current(ctx, s) != nil {
		return result, iyuu.ErrInput
	}
	created := false
	err = api.bindingTx(ctx, s, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO GO_IYUU_BINDINGS(ID,ACCOUNT,INTENT,SITE,STATE) VALUES(?,?,?,?,?) ON CONFLICT(ID) DO NOTHING", result.ID, s.account(), intent, result.Site, result.State)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		return read(tx)
	})
	if err != nil || !created {
		return result, err
	}
	state := "rejected"
	if api.current(ctx, s) == nil {
		switch err = s.client.Bind(ctx, partner, in.UID, in.Passkey); {
		case err == nil:
			state = "bound"
		case !errors.Is(err, iyuu.ErrRejected):
			state = "unknown"
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err = api.transfer.system.Transaction(save, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(save, "UPDATE GO_IYUU_BINDINGS SET STATE=? WHERE ID=? AND INTENT=? AND STATE='pending'", state, result.ID, intent)
		return err
	})
	result.State = state
	return result, err
}

// The original partner directory was visible to signed-in users in mixed
// deployments. Binding and receipt access have their own administrator check.
func (service pluginService) iyuuPartnerPage(w http.ResponseWriter, r *http.Request, legacy bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if service.iyuu.transfer.login.pureGo {
		scope, ok := service.iyuu.requestScope(w, r, false)
		if !ok {
			return
		}
		if partners, ok := service.iyuu.checkedPartners(w, r, scope); ok {
			writeIyuuPartnerPage(w, partners, legacy)
		}
		return
	}
	if _, ok := service.catalogReader(w, r); !ok {
		return
	}
	ctx := r.Context()
	keys := []string{"UserInstalledPlugins", iyuu.ConfigKey, iyuuGeneration}
	before, err := service.system.ReadMany(ctx, keys)
	if err != nil {
		writeAPIError(w, 503, 503, "读取 IYUU 配置失败")
		return
	}
	installed, err := indexerInstalled(before["UserInstalledPlugins"], iyuu.Plugin)
	if err != nil {
		writeAPIError(w, 503, 503, "无法读取已安装插件")
		return
	}
	if !installed {
		writeAPIError(w, 404, 404, "尚未安装 IYUU 插件")
		return
	}
	token, err := iyuu.BindingToken(before[iyuu.ConfigKey])
	if err != nil {
		writeAPIError(w, 409, 409, "请核对 IYUU 插件配置")
		return
	}
	if token == "" {
		if _, command := w.(pluginCommandPageWriter); command {
			writeJSON(w, 200, map[string]any{"code": 0, "title": nil, "content": nil, "func": nil})
		} else {
			writeAPIError(w, 404, 404, "请先在 IYUU 插件配置中保存 Token")
		}
		return
	}
	client := iyuu.Client{Token: token, Transport: service.iyuu.transport}
	partners, err := client.Partners(ctx)
	if err != nil {
		writeAPIError(w, 502, 502, "读取 IYUU 合作站点失败")
		return
	}
	if _, ok := service.catalogReader(w, r); !ok {
		return
	}
	after, err := service.system.ReadMany(ctx, keys)
	if err != nil || !maps.Equal(before, after) {
		writeAPIError(w, 409, 409, "IYUU 设置已变化，请刷新")
		return
	}
	writeIyuuPartnerPage(w, partners, legacy)
}

func writeIyuuPartnerPage(w http.ResponseWriter, partners []iyuu.Partner, legacy bool) {
	table := pluginPageTable{Columns: []string{"合作站点", "站点代码", "编号"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, v := range partners {
		table.Rows = append(table.Rows, []string{v.Nickname, v.Site, v.ID.String()})
	}
	writeNativeRecordPage(w, pluginPageData{Title: "IYUU 站点绑定", Sections: []string{}, Tables: []pluginPageTable{table}, ReadOnly: true, ActionsOmitted: true}, legacy)
}

func (api *iyuuAPI) legacyBind(w http.ResponseWriter, r *http.Request, body json.RawMessage) bool {
	var in struct {
		PluginID                        string `json:"plugin_id"`
		Method, Site, SID, UID, Passkey string
	}
	if json.Unmarshal(body, &in) != nil || in.PluginID != iyuu.Plugin {
		return false
	}
	if in.Method != "iyuu_bind_site" {
		writeAPIError(w, 400, 400, "不支持的 IYUU 操作")
		return true
	}
	s, ok := api.requestScope(w, r, false)
	if !ok {
		return true
	}
	id := sitelogin.Hash([]string{s.account(), in.Site, in.SID, in.UID, in.Passkey})[:32]
	result, err := api.bindSite(r.Context(), s, iyuuBinding{Fingerprint: s.proof, RequestID: id, Site: in.Site, SID: in.SID, UID: in.UID, Passkey: in.Passkey})
	code, msg := 1, "绑定结果未确认，请打开插件查看记录"
	if err == nil && result.State == "bound" {
		code, msg = 0, "绑定成功"
	} else if result.State == "rejected" {
		msg = "绑定被拒绝，请核对账户资料"
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]any{"code": code, "msg": msg}})
	return true
}
