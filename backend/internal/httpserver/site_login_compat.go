package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func (api *siteLoginAPI) legacyAction(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			writeAPIError(w, 400, 400, "原操作报文过大")
			return
		}
		var envelope struct {
			Cmd  string          `json:"cmd"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "原操作格式无效")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if envelope.Cmd != "update_sites_cookie_ua" && envelope.Cmd != "set_site_captcha_code" && envelope.Cmd != "update_site_cookie_ua" {
			next(w, r)
			return
		}
		actor, ok := api.actor(w, r, true)
		if !ok {
			return
		}
		switch envelope.Cmd {
		case "update_sites_cookie_ua":
			api.legacyStart(w, r, actor, envelope.Data)
		case "set_site_captcha_code":
			var in struct{ Code, Value string }
			if json.Unmarshal(envelope.Data, &in) != nil {
				writeAPIError(w, 400, 400, "原验证信息格式无效")
				return
			}
			id := ""
			api.mu.Lock()
			for job, p := range api.prompts {
				if p.nonce == in.Code {
					id = job
					break
				}
			}
			api.mu.Unlock()
			if id == "" {
				writeAPIError(w, 409, 409, "原验证请求已失效")
				return
			}
			cloned := r.Clone(r.Context())
			cloned.SetPathValue("id", id)
			body, _ := json.Marshal(map[string]string{"Nonce": in.Code, "Value": in.Value})
			cloned.Body = io.NopCloser(bytes.NewReader(body))
			api.serveInput(w, cloned)
		case "update_site_cookie_ua":
			var in struct {
				ID     string `json:"site_id"`
				Cookie string `json:"site_cookie"`
				Agent  string `json:"site_ua"`
			}
			if json.Unmarshal(envelope.Data, &in) != nil {
				writeAPIError(w, 400, 400, "原 Cookie 更新格式无效")
				return
			}
			id, err := strconv.ParseInt(in.ID, 10, 64)
			if err != nil || id <= 0 || !sitelogin.Text(in.Cookie, 16000) || !sitelogin.Text(in.Agent, 512) {
				writeAPIError(w, 400, 400, "原 Cookie 更新参数无效")
				return
			}
			if err = api.sites.UpdateCookieAndAgent(r.Context(), id, in.Cookie, in.Agent); err != nil {
				writeAPIError(w, 409, 409, "原站点未能确认保存")
				return
			}
			writeJSON(w, 200, map[string]any{"code": 0, "messages": "原站点 Cookie 和 User-Agent 已保存"})
		}
	}
}
func (api *siteLoginAPI) legacyStart(w http.ResponseWriter, r *http.Request, actor string, raw json.RawMessage) {
	var in struct {
		SiteID    json.RawMessage `json:"siteid"`
		Username  string          `json:"username"`
		Password  string          `json:"password"`
		TwoStep   string          `json:"two_step_code"`
		OCR       json.RawMessage `json:"ocrflag"`
		RequestID string          `json:"request_id"`
	}
	if json.Unmarshal(raw, &in) != nil || !sitelogin.Text(in.Username, 1024) || !sitelogin.Text(in.Password, 4096) || !sitelogin.Text(in.TwoStep, 128) || in.Username == "" || in.Password == "" || !sitelogin.Text(in.RequestID, 128) {
		writeAPIError(w, 400, 400, "原登录参数无效")
		return
	}
	ids := []string{}
	id := ""
	if len(in.SiteID) > 0 && string(in.SiteID) != "null" {
		if json.Unmarshal(in.SiteID, &id) != nil {
			var number json.Number
			if json.Unmarshal(in.SiteID, &number) != nil {
				writeAPIError(w, 400, 400, "原站点编号无效")
				return
			}
			id = number.String()
		}
	}
	if id != "" {
		ids = append(ids, id)
	} else {
		sites, err := api.sites.List(r.Context())
		if err != nil {
			writeAPIError(w, 503, 503, "原站点列表不可用")
			return
		}
		for _, site := range sites {
			if site.SignURL != "" || site.RSSURL != "" {
				ids = append(ids, strconv.FormatInt(site.ID, 10))
			}
		}
	}
	useOCR := false
	if len(in.OCR) > 0 && string(in.OCR) != "null" {
		var value any
		if json.Unmarshal(in.OCR, &value) != nil {
			writeAPIError(w, 400, 400, "原 OCR 参数无效")
			return
		}
		useOCR = value == true || value == "true" || value == "Y" || value == float64(1)
	}
	review, err := api.review(r.Context(), actor, siteLoginOptions{SiteIDs: ids, Remember: true, OCR: useOCR})
	if err != nil {
		writeAPIError(w, 409, 409, "原登录目标或 OCR 配置无法确认")
		return
	}
	plan, err := api.unseal(actor, review.ScopeToken)
	if err != nil {
		writeAPIError(w, 409, 409, "原登录审核不可用")
		return
	}
	key, err := api.planKey(actor)
	if err != nil {
		writeAPIError(w, 403, 403, "当前登录权限已变化")
		return
	}
	// Older clients have no request nonce. Identical unversioned calls must
	// still read the original receipt after Cookie publication or a restart.
	// An explicit request_id (or the React review flow) starts a new attempt.
	marker := in.RequestID
	if marker == "" {
		marker = "original-unversioned-request"
	}
	signed, _ := json.Marshal([]any{"legacy-site-login", actor, plan.Options.SiteIDs, in.Username, in.Password, in.TwoStep, useOCR, marker})
	mac := hmac.New(sha256.New, key)
	mac.Write(signed)
	plan.ID = hex.EncodeToString(mac.Sum(nil))
	records, err := api.launch(r.Context(), actor, r.Header.Get("Authorization"), plan, sitelogin.Credentials{Username: in.Username, Password: in.Password, TwoStep: in.TwoStep})
	if err != nil {
		writeAPIError(w, 409, 409, "原登录请求无法创建，已有请求请按原编号查询")
		return
	}
	_ = records
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		batch, items, err := api.store.Batch(r.Context(), plan.ID)
		if err != nil {
			writeAPIError(w, 503, 503, "原请求已创建，请只读查询编号 "+plan.ID)
			return
		}
		complete := batch.RememberState != "pending"
		ret := 0
		messages := []string{}
		for _, item := range items {
			if sitelogin.Active(item.State) {
				complete = false
			}
			if item.State != "completed" {
				ret = 1
			}
			messages = append(messages, item.Name+" "+item.State+" "+item.Reason)
		}
		if complete {
			writeJSON(w, 200, map[string]any{"code": ret, "messages": messages, "batch_id": plan.ID})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (api *siteLoginAPI) latest(ctx context.Context, actor string) (sitelogin.Batch, []sitelogin.Record, error) {
	// The newest record carries the original batch ID; don't create progress
	// state or start work merely because a progress consumer connected.
	records, err := api.store.List(ctx, 1)
	if err != nil {
		return sitelogin.Batch{}, nil, err
	}
	for _, r := range records {
		if r.Actor == actor {
			return api.store.Batch(ctx, r.Batch)
		}
	}
	return sitelogin.Batch{}, nil, nil
}
func (api *siteLoginAPI) legacyProgress(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("type") != "sitecookie" {
		writeAPIError(w, 501, 501, "该进度类型尚未迁移")
		return
	}
	actor, ok := api.actor(w, r, false)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 503, 503, "当前连接不支持进度流")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		b, records, err := api.latest(r.Context(), actor)
		if err != nil {
			return
		}
		completed := 0
		message := "暂无站点登录请求"
		active := false
		for _, record := range records {
			view := api.view(r.Context(), record, actor, r.Header.Get("Authorization"))
			if !sitelogin.Active(record.State) {
				completed++
			} else {
				active = true
				message = record.Name + " " + record.State
			}
			if c := view.Challenge; c != nil {
				switch c.Kind {
				case "captcha":
					message = c.Image + "|" + c.Nonce
				case "email":
					message = "data:email|" + c.Nonce
				case "email_code":
					message = "data:email_verify_code|" + c.Nonce
				case "two_step":
					message = "data:two_step_code|" + c.Nonce
				}
				break
			}
		}
		value := 0
		if len(records) > 0 {
			value = completed * 100 / len(records)
		}
		payload, _ := json.Marshal(map[string]any{"code": 0, "value": value, "text": message, "enable": active, "batch_id": b.ID})
		if _, err = fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return
		}
		flusher.Flush()
		if !active && len(records) > 0 {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
