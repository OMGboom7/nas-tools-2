package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

// Keep this projection on the server: it contains credentials needed for
// merging updates. Only safeSiteDetail/siteSummary may reach React.
func nativeSiteRecord(item siteconfig.Site) (map[string]any, error) {
	note := map[string]any{}
	if item.Note != "" && item.Note != "null" {
		if err := json.Unmarshal([]byte(item.Note), &note); err != nil {
			return nil, errors.New("invalid stored site attributes")
		}
	}
	rss := strings.Contains(item.Include, "D") && item.RSSURL != ""
	brush := strings.Contains(item.Include, "S") && item.RSSURL != "" && item.Cookie != ""
	statistic := strings.Contains(item.Include, "T") && (item.SignURL != "" || item.RSSURL != "") && item.Cookie != ""
	uses := make([]any, 0, 3)
	for _, flag := range []struct {
		code    string
		enabled bool
	}{{"D", rss}, {"S", brush}, {"T", statistic}} {
		if flag.enabled {
			uses = append(uses, flag.code)
		}
	}
	record := map[string]any{
		"id": item.ID, "name": item.Name, "pri": item.Priority,
		"rssurl": item.RSSURL, "signurl": item.SignURL, "cookie": item.Cookie, "api_key": item.APIKey,
		"rss_enable": rss, "brush_enable": brush, "statistic_enable": statistic, "uses": uses,
		"strict_url": safeSiteOrigin(defaultString(item.SignURL, item.RSSURL)),
		"parse":      note["parse"] == "Y", "unread_msg_notify": note["message"] == "Y",
		"chrome": note["chrome"] == "Y", "proxy": note["proxy"] == "Y", "subtitle": note["subtitle"] == "Y",
	}
	for _, key := range []string{"rule", "download_setting", "ua", "tags", "limit_interval", "limit_count", "limit_seconds"} {
		record[key] = note[key]
	}
	return record, nil
}

func (service siteService) nativeSiteList(ctx context.Context, rssOnly bool) ([]any, error) {
	items, err := service.store.List(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]any, 0, len(items))
	for _, item := range items {
		record, err := nativeSiteRecord(item)
		if err != nil {
			return nil, err
		}
		if rssOnly && !truthy(record["rss_enable"]) {
			continue
		}
		values = append(values, record)
	}
	return values, nil
}

func (service siteService) saveNativeSite(ctx context.Context, form url.Values) error {
	id, _ := strconv.ParseInt(form.Get("site_id"), 10, 64)
	item := siteconfig.Site{}
	if id != 0 {
		current, err := service.store.Get(ctx, id)
		if err != nil {
			return err
		}
		item = current
	}
	// Retain unexposed attributes and legacy columns during a React edit.
	note := map[string]any{}
	if item.Note != "" && item.Note != "null" {
		if err := json.Unmarshal([]byte(item.Note), &note); err != nil {
			return err
		}
	}
	changes := map[string]any{}
	if err := json.Unmarshal([]byte(form.Get("site_note")), &changes); err != nil {
		return err
	}
	for key, value := range changes {
		note[key] = value
	}
	encoded, err := json.Marshal(note)
	if err != nil {
		return err
	}
	item.ID, item.Name, item.Priority = id, form.Get("site_name"), form.Get("site_pri")
	item.RSSURL, item.SignURL = form.Get("site_rssurl"), form.Get("site_signurl")
	item.Cookie, item.APIKey = form.Get("site_cookie"), form.Get("site_api_key")
	item.Include, item.Note = form.Get("site_include"), string(encoded)
	_, err = service.store.Upsert(ctx, item)
	return err
}

func writeNativeSiteError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, siteconfig.ErrNotFound):
		writeAPIError(response, http.StatusNotFound, 404, "站点不存在")
	case errors.Is(err, siteconfig.ErrDuplicate):
		writeAPIError(response, http.StatusConflict, 409, "站点名称重复")
	default:
		writeAPIError(response, http.StatusInternalServerError, 500, "站点数据库操作失败")
	}
}
