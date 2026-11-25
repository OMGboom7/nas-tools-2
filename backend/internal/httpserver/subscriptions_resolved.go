package httpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

const subscriptionColumns = "NAME,YEAR,TMDBID,IMAGE,RSS_SITES,SEARCH_SITES,OVER_EDITION,FILTER_RESTYPE,FILTER_PIX,FILTER_RULE,FILTER_TEAM,FILTER_INCLUDE,FILTER_EXCLUDE,SAVE_PATH,DOWNLOAD_SETTING,FUZZY_MATCH,STATE,DESC,NOTE,KEYWORD"
const tvSubscriptionColumns = ",SEASON,TOTAL_EP,CURRENT_EP,TOTAL,LACK"

type resolvedSubscription struct {
	Kind, EditID, DefaultKind string
	Fields                    map[string]any
	ChooseSeason              bool
	Season, EndSeason         *int
	SourceID                  string
	Douban                    bool
	Seasons                   []struct {
		Number int `json:"season_number"`
		Count  int `json:"episode_count"`
	}
	Choice *subscriptionChoice
}

type subscriptionChoice struct {
	Channel, Sender, Revision, Selector string
	Index                               int
}

type subscriptionWriteReceipt struct {
	Code     int            `json:"code"`
	Message  string         `json:"message"`
	ID       int64          `json:"id,omitempty"`
	Season   *int           `json:"season,omitempty"`
	Episodes int            `json:"episodes,omitempty"`
	Event    map[string]any `json:"event,omitempty"`
}

func ResolvedSubscriptionCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var req resolvedSubscription
	input := &io.LimitedReader{R: in, N: 2 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	d.UseNumber()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || (req.Kind != "MOV" && req.Kind != "TV") || req.Fields == nil {
		return errors.New("invalid resolved subscription")
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	if st, e := os.Stat(dbPath); e != nil || !st.Mode().IsRegular() {
		return errors.New("subscription database unavailable")
	}
	db, err := (subscriptionService{databasePath: dbPath}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	receipt, err := req.save(ctx, tx, path)
	if err != nil {
		return err
	}
	if receipt.Code == 0 {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(receipt)
}

func (req *resolvedSubscription) save(ctx context.Context, tx *sql.Tx, configPath string) (subscriptionWriteReceipt, error) {
	f := req.Fields
	fail := func(code int, msg string) (subscriptionWriteReceipt, error) {
		return subscriptionWriteReceipt{Code: code, Message: msg}, nil
	}
	if strings.TrimSpace(text(f["NAME"])) == "" {
		return fail(-1, "标题或类型有误")
	}
	if req.Choice != nil {
		ok, err := req.Choice.current(ctx, tx, req)
		if err != nil {
			return subscriptionWriteReceipt{}, err
		}
		if !ok {
			return fail(4, "选片已失效，请重新选择")
		}
		if text(f["STATE"]) == "R" {
			settings, err := config.NewStore(configPath).Snapshot()
			if err != nil {
				return subscriptionWriteReceipt{}, err
			}
			if !truthy(objectValue(settings["pt"])["search_no_result_rss"]) {
				return fail(4, "自动订阅已关闭")
			}
		}
	}
	if req.DefaultKind != "" {
		if req.DefaultKind != "MOV" && req.DefaultKind != "TV" {
			return fail(-1, "订阅默认设置有误")
		}
		var stored string
		err := tx.QueryRowContext(ctx, "SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1", "DefaultRssSetting"+req.DefaultKind).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return subscriptionWriteReceipt{}, err
		}
		if err := applyResolvedDefaults(stored, f); err != nil {
			return subscriptionWriteReceipt{}, err
		}
	}
	receipt := subscriptionWriteReceipt{Message: "添加订阅成功"}
	if req.ChooseSeason {
		season := req.Season
		if season == nil && req.Douban {
			one := 1
			season = &one
		}
		if season == nil {
			for _, s := range req.Seasons {
				if s.Number > 0 && (season == nil || s.Number > *season) {
					n := s.Number
					season = &n
				}
			}
		}
		if season == nil {
			return fail(2, "获取剧集信息失败")
		}
		for _, s := range req.Seasons {
			if s.Number == *season {
				receipt.Episodes = s.Count
				break
			}
		}
		if receipt.Episodes <= 0 {
			return fail(3, fmt.Sprintf("第%d季获取剧集数失败，请确认该季是否存在", *season))
		}
		receipt.Season = season
		total := receipt.Episodes
		if n, _ := strconv.Atoi(text(f["TOTAL_EP"])); n > 0 {
			total = n
		}
		lack := total
		if start, _ := strconv.Atoi(text(f["CURRENT_EP"])); start > 0 {
			lack = total - start - 1
		}
		f["SEASON"], f["TOTAL"], f["LACK"] = fmt.Sprintf("S%02d", *season), total, lack
		if req.EndSeason != nil {
			f["SEASON"] = fmt.Sprintf("S%02d-S%02d", *season, *req.EndSeason)
		}
	}
	event := map[string]any{"rssid": nil}
	for k, col := range map[string]string{"rss_sites": "RSS_SITES", "search_sites": "SEARCH_SITES", "over_edition": "OVER_EDITION", "filter_restype": "FILTER_RESTYPE", "filter_pix": "FILTER_PIX", "filter_team": "FILTER_TEAM", "filter_rule": "FILTER_RULE", "save_path": "SAVE_PATH", "download_setting": "DOWNLOAD_SETTING", "total_ep": "TOTAL_EP", "current_ep": "CURRENT_EP", "keyword": "KEYWORD"} {
		event[k] = f[col]
	}
	event["fuzzy_match"] = truthy(f["FUZZY_MATCH"])
	for _, key := range []string{"RSS_SITES", "SEARCH_SITES"} {
		sites := f[key]
		if sites == nil {
			sites = []string{}
			event[strings.ToLower(key)] = sites
		}
		raw, err := json.Marshal(sites)
		if err != nil {
			return subscriptionWriteReceipt{}, err
		}
		f[key] = string(raw)
	}
	columns := subscriptionColumns
	if req.Kind == "TV" {
		columns += tvSubscriptionColumns
	}
	row := subscriptionRow{ID: req.EditID, Kind: req.Kind, Columns: columns, Name: f["NAME"], Year: f["YEAR"], Season: f["SEASON"], AllSeasons: req.Kind == "TV" && text(f["SEASON"]) == "", ClearFilterOrder: true}
	for _, key := range strings.Split(columns, ",") {
		row.Values = append(row.Values, f[key])
	}
	id, err := writeSubscriptionRow(ctx, tx, row)
	if errors.Is(err, errSubscriptionAlreadyExists) {
		receipt.Code, receipt.Message = 9, "订阅已存在"
		return receipt, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fail(-1, "所编辑的订阅已不存在")
	}
	if err != nil {
		return subscriptionWriteReceipt{}, err
	}
	receipt.ID, receipt.Event = id, event
	return receipt, nil
}

func applyResolvedDefaults(stored string, f map[string]any) error {
	var defaults subscriptionUpsertRequest
	if err := applyStoredSubscriptionDefaults(stored, &defaults); err != nil {
		return err
	}
	for key, value := range map[string]string{"FILTER_RESTYPE": defaults.Quality, "FILTER_PIX": defaults.Resolution, "FILTER_TEAM": defaults.ReleaseGroup, "FILTER_INCLUDE": defaults.Include, "FILTER_EXCLUDE": defaults.Exclude} {
		if text(f[key]) == "" && value != "" {
			f[key] = value
		}
	}
	for key, value := range map[string]string{"FILTER_RULE": defaults.FilterRule, "DOWNLOAD_SETTING": defaults.DownloadSetting} {
		n, err := strconv.ParseInt(value, 10, 64)
		if (text(f[key]) == "" || text(f[key]) == "0") && err == nil && (key == "DOWNLOAD_SETTING" || n >= 0) {
			f[key] = n
		}
	}
	if !truthy(f["OVER_EDITION"]) && defaults.OverEdition {
		f["OVER_EDITION"] = 1
	}
	for key, sites := range map[string][]string{"RSS_SITES": defaults.RSSSites, "SEARCH_SITES": defaults.SearchSites} {
		if len(stringsOf(f[key])) == 0 && len(sites) > 0 {
			f[key] = sites
		}
	}
	return nil
}

// Verify the original menu and the current receiver while holding the same
// SQLite writer reservation as the subscription insert.
func (c subscriptionChoice) current(ctx context.Context, tx *sql.Tx, req *resolvedSubscription) (bool, error) {
	state := text(req.Fields["STATE"])
	var mode, rev string
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT ACTION,REVISION,CHOICES FROM GO_CHAT_CHOICE WHERE CHANNEL=? AND SENDER=?", c.Channel, c.Sender).Scan(&mode, &rev, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var choices []json.RawMessage
	if rev != c.Revision || c.Index < 1 || json.Unmarshal(raw, &choices) != nil || c.Index > len(choices) || !(mode == "subscribe" && state == "D" || mode == "search" && state == "R") {
		return false, nil
	}
	var picked struct {
		TMDB    json.RawMessage `json:"tmdb_id"`
		Douban  json.RawMessage `json:"douban_id"`
		Kind    string
		Season  *int            `json:"begin_season"`
		RSS     []string        `json:"rss_sites"`
		Search  []string        `json:"search_sites"`
		Setting json.RawMessage `json:"download_setting"`
	}
	if json.Unmarshal(choices[c.Index-1], &picked) != nil {
		return false, nil
	}
	selector := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return string(raw)
	}
	id := selector(picked.TMDB)
	if dbID := selector(picked.Douban); dbID != "" && dbID != "0" {
		id = "DB:" + dbID
	}
	defaultKind := "MOV"
	if picked.Kind == "电视剧" {
		defaultKind = "TV"
	}
	if id == "" || id != c.Selector || id != req.SourceID || (picked.Kind == "电影") != (req.Kind == "MOV") ||
		req.DefaultKind != defaultKind || req.ChooseSeason != (req.Kind == "TV") || req.EditID != "" || truthy(req.Fields["FUZZY_MATCH"]) {
		return false, nil
	}
	if !strings.HasPrefix(id, "DB:") && id != text(req.Fields["TMDBID"]) {
		return false, nil
	}
	if picked.Season != nil && *picked.Season != 0 && (req.Season == nil || *picked.Season != *req.Season) {
		return false, nil
	}
	if !slices.Equal(picked.RSS, stringsOf(req.Fields["RSS_SITES"])) || !slices.Equal(picked.Search, stringsOf(req.Fields["SEARCH_SITES"])) {
		return false, nil
	}
	setting, requested := selector(picked.Setting), text(req.Fields["DOWNLOAD_SETTING"])
	if setting == "0" {
		setting = ""
	}
	if requested == "0" {
		requested = ""
	}
	if setting != requested {
		return false, nil
	}
	source, scope, ok := strings.Cut(c.Channel, "/")
	if !ok {
		return false, nil
	}
	if source == "WEB" {
		return scope == "" && c.Sender != "", nil
	}
	kind := map[string]string{"TG": "telegram", "WX": "wechat", "SLACK": "slack", "SYNOLOGY": "synologychat"}[source]
	if kind == "" {
		return false, nil
	}
	var channelID int64
	var configuration string
	err = tx.QueryRowContext(ctx, `SELECT ID,CONFIG FROM MESSAGE_CLIENT WHERE TYPE=? AND INTERACTIVE<>0 AND ENABLED<>0
	AND (SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE TYPE=? AND INTERACTIVE<>0 AND ENABLED<>0)=1`, kind, kind).Scan(&channelID, &configuration)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if source != "TG" {
		return scope == strconv.FormatInt(channelID, 10), nil
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(configuration), &cfg) != nil {
		return false, nil
	}
	token := text(cfg["token"])
	return validTelegramToken(token) && scope == fmt.Sprintf("%d/%x", channelID, sha256.Sum256([]byte(token))) && telegramSenderAllowed(cfg, c.Sender, ""), nil
}
