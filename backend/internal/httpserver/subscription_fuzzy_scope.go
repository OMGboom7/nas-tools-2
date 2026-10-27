package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

var errFuzzyScope = errors.New("original fuzzy subscription authority or receipt changed")
var errFuzzyMissing = errors.New("fuzzy subscription receipt not found")

type fuzzyReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type fuzzySubscriptionAPI struct {
	runner  *subscriptionSearchRunner
	records *organization.Store
	workers atomic.Int32
}
type fuzzyGrant struct {
	Version                                         int
	Kind                                            string
	ID                                              int64
	Epoch, Actor, Authority, Source, Scope, Created string
	Authorized                                      bool
}
type fuzzySourceSnapshot struct {
	raw                                             map[string]any
	grant                                           fuzzyGrant
	grantRaw, source, scope, authority, fingerprint string
}
type fuzzyGrantReview struct {
	Kind          string             `json:"kind"`
	ID            int64              `json:"id,string"`
	Name          string             `json:"name"`
	Year          string             `json:"year"`
	Season        string             `json:"season"`
	OverEdition   bool               `json:"overEdition"`
	Actor         string             `json:"actor"`
	Active        bool               `json:"active"`
	Fingerprint   string             `json:"fingerprint"`
	Sites         []string           `json:"sites"`
	WorkerRunning bool               `json:"workerRunning"`
	Targets       []fuzzyGrantTarget `json:"targets"`
}
type fuzzyGrantTarget struct {
	Site         string `json:"site"`
	DownloaderID string `json:"downloaderId"`
	Downloader   string `json:"downloader"`
	Type         string `json:"type"`
	Directory    string `json:"directory"`
	Paused       bool   `json:"paused"`
}

func fuzzyDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func fuzzyHex(v string, n int) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(v) == n && hex.EncodeToString(b) == v
}
func fuzzyTable(ctx context.Context, r fuzzyReader, name string) (bool, error) {
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, err
}
func fuzzyRows(ctx context.Context, r fuzzyReader, table string) (any, error) {
	yes, err := fuzzyTable(ctx, r, table)
	if err != nil || !yes {
		return nil, err
	}
	// Table names come exclusively from the fixed configuration list below.
	rows, err := r.QueryContext(ctx, `SELECT * FROM "`+table+`"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) > 256 {
		return nil, errFuzzyScope
	}
	records := []string{}
	bytes := 0
	for rows.Next() {
		v := make([]any, len(columns))
		p := make([]any, len(columns))
		for i := range p {
			p[i] = &v[i]
		}
		if err := rows.Scan(p...); err != nil {
			return nil, err
		}
		for i := range v {
			v[i] = fuzzySQLCell(v[i])
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		bytes += len(b)
		if len(records) >= 10000 || bytes > 8<<20 {
			return nil, errFuzzyScope
		}
		records = append(records, string(b))
	}
	sort.Strings(records)
	return []any{columns, records}, rows.Err()
}
func (api *fuzzySubscriptionAPI) service() subscriptionService {
	return api.runner.planner.runner.recognition.service
}
func (api *fuzzySubscriptionAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.runner == nil || api.runner.planner.runner.download.service.auth == nil {
		writeAPIError(w, 503, 503, "native fuzzy subscription unavailable")
		return "", false
	}
	auth := api.runner.planner.runner.download.service.auth.service
	claims, err := auth.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return "", false
	}
	if !auth.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "current administrator required")
		return "", false
	}
	if !api.runner.planner.runner.pureGo {
		writeAPIError(w, 501, 501, "fuzzy subscription execution requires disabled legacy backend")
		return "", false
	}
	if api.records == nil {
		writeAPIError(w, 503, 503, "native fuzzy receipt store unavailable")
		return "", false
	}
	if _, err := api.authority(claims.Username); err != nil {
		writeAPIError(w, 403, 403, "current administrator required")
		return "", false
	}
	return claims.Username, true
}
func (api *fuzzySubscriptionAPI) authority(actor string) (string, error) {
	snapshot, err := api.service().configStore.Snapshot()
	if err != nil {
		return "", err
	}
	app := objectValue(snapshot["app"])
	if actor == "" || text(app["login_user"]) != actor || !api.runner.planner.runner.download.service.auth.service.IsAdministrator(actor) {
		return "", errFuzzyScope
	}
	return fuzzyDigest([]any{actor, app["login_password"], objectValue(snapshot["security"])["api_key"]}), nil
}
func (api *fuzzySubscriptionAPI) configuration(ctx context.Context, r fuzzyReader) (string, error) {
	return nativeSubscriptionConfiguration(ctx, r, api.service())
}
func nativeSubscriptionConfiguration(ctx context.Context, r fuzzyReader, service subscriptionService) (string, error) {
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	// Authentication is bound separately. Fresh explicit approval may change
	// the administrator, never the batch's original downloader/site scope.
	for key, excluded := range map[string]map[string]bool{
		"app":      {"login_user": true, "login_password": true},
		"security": {"api_key": true},
	} {
		copy := map[string]any{}
		for name, value := range objectValue(configuration[key]) {
			if !excluded[name] {
				copy[name] = value
			}
		}
		configuration[key] = copy
	}
	parts := map[string]any{"yaml": configuration}
	for _, name := range []string{"DOWNLOADER", "DOWNLOAD_SETTING", "CONFIG_SITE", "CONFIG_FILTER_GROUP", "CONFIG_FILTER_RULES", "CUSTOM_WORD_GROUPS", "CUSTOM_WORDS"} {
		v, err := fuzzyRows(ctx, r, name)
		if err != nil {
			return "", err
		}
		parts[name] = v
	}
	settings := map[string]string{}
	for _, key := range []string{"DefaultDownloader", "DefaultDownloadSetting", "UserInstalledPlugins", "UserIndexerSites", "plugin.Prowlarr", "plugin.Jackett", "plugin.CustomReleaseGroups", "plugin.Customization"} {
		var value string
		err := r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		settings[key] = value
	}
	parts["settings"] = settings
	// Detail parsing reads the bundled catalog. Freeze its bytes as well as
	// its path, rather than silently adopting a replacement after review.
	path := service.catalogPath
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		b, readErr := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(b) > 16<<20 {
			return "", errFuzzyScope
		}
		sum := sha256.Sum256(b)
		parts["catalog"] = []string{path, hex.EncodeToString(sum[:])}
	}
	result := fuzzyDigest(parts)
	if !fuzzyHex(result, 64) {
		return "", errFuzzyScope
	}
	return result, nil
}
func readFuzzyGrant(ctx context.Context, r fuzzyReader, kind string, id int64) (fuzzyGrant, string, error) {
	g := fuzzyGrant{}
	yes, err := fuzzyTable(ctx, r, "GO_FUZZY_SUBSCRIPTION_GRANTS")
	if err != nil || !yes {
		return g, "", err
	}
	var raw string
	err = r.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_FUZZY_SUBSCRIPTION_GRANTS WHERE KIND=? AND SUB_ID=?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return g, "", nil
	}
	if err != nil {
		return g, "", err
	}
	if decodeFuzzyDocument(raw, &g, 64<<10) != nil || g.Version != 1 || g.Kind != kind || g.ID != id || !fuzzyHex(g.Epoch, 32) || g.Actor == "" || !fuzzyHex(g.Source, 64) || !fuzzyHex(g.Scope, 64) || !fuzzyHex(g.Authority, 64) {
		return g, raw, errFuzzyScope
	}
	if _, err := time.Parse(time.RFC3339Nano, g.Created); err != nil {
		return g, raw, errFuzzyScope
	}
	return g, raw, nil
}
func (api *fuzzySubscriptionAPI) source(ctx context.Context, r fuzzyReader, kind string, id int64, actor string) (fuzzySourceSnapshot, error) {
	s := fuzzySourceSnapshot{}
	if id < 1 || kind != "MOV" && kind != "TV" {
		return s, errFuzzyScope
	}
	var err error
	s.raw, err = readFuzzySourceRow(ctx, r, kind, id)
	if err != nil {
		return s, err
	}
	if s.raw == nil {
		return s, errFuzzyMissing
	}
	if !truthy(normalizeNativeSubscriptionRow(s.raw)["fuzzy_match"]) {
		return s, errFuzzyScope
	}
	s.source = fuzzyDigest(s.raw)
	s.authority, err = api.authority(actor)
	if err != nil {
		return s, err
	}
	s.scope, err = api.configuration(ctx, r)
	if err != nil {
		return s, err
	}
	s.grant, s.grantRaw, err = readFuzzyGrant(ctx, r, kind, id)
	if err != nil {
		return s, err
	}
	s.fingerprint = fuzzyDigest([]any{kind, id, s.source, s.scope, s.authority, s.grantRaw})
	return s, nil
}
func fuzzyGrantCurrent(s fuzzySourceSnapshot, g fuzzyGrant) bool {
	return g.Authorized && g == s.grant && g.Source == s.source && g.Scope == s.scope && g.Authority == s.authority && text(s.raw["STATE"]) == "R"
}
func verifyFuzzySource(ctx context.Context, r fuzzyReader, kind string, id int64, raw map[string]any) error {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	query, args := "ID=?", []any{id}
	for _, key := range keys {
		query += ` AND "` + strings.ReplaceAll(key, `"`, `""`) + `" IS ?`
		value, err := fuzzySQLValue(raw[key])
		if err != nil {
			return err
		}
		args = append(args, value)
	}
	var yes bool
	if err := r.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+subscriptionTable(kind)+" WHERE "+query+")", args...).Scan(&yes); err != nil {
		return err
	}
	if !yes {
		return errFuzzyScope
	}
	return nil
}

// Private raw snapshots preserve NULL, BLOB and SQLite REAL values even for
// user-added columns; no secret or raw row is returned in the public review.
func fuzzySQLCell(v any) any {
	switch x := v.(type) {
	case string:
		if !utf8.ValidString(x) {
			return map[string]any{"$sqlite_text_bytes": hex.EncodeToString([]byte(x))}
		}
	case []byte:
		return map[string]any{"$sqlite_blob": hex.EncodeToString(x)}
	case int64:
		// Keep full SQLite integer precision while sharing the native
		// normalizer's JSON-number boolean and numeric interpretation.
		return json.Number(strconv.FormatInt(x, 10))
	case float64:
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return json.Number(s)
	}
	return v
}
func fuzzySQLValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if n, e := x.Int64(); e == nil {
			return n, nil
		}
		n, e := x.Float64()
		return n, e
	case map[string]any:
		if value, ok := x["$sqlite_text_bytes"].(string); ok && len(x) == 1 {
			b, e := hex.DecodeString(value)
			return string(b), e
		}
		value, ok := x["$sqlite_blob"].(string)
		if !ok || len(x) != 1 {
			return nil, errFuzzyScope
		}
		b, e := hex.DecodeString(value)
		return b, e
	case nil, string, int, int64, float64:
		return v, nil
	default:
		return nil, errFuzzyScope
	}
}
func readFuzzySourceRow(ctx context.Context, r fuzzyReader, kind string, id int64) (map[string]any, error) {
	return readFuzzyTableRow(ctx, r, subscriptionTable(kind), id)
}
func readFuzzyTableRow(ctx context.Context, r fuzzyReader, table string, id int64) (map[string]any, error) {
	if table != "RSS_MOVIES" && table != "RSS_TVS" && table != "RSS_HISTORY" {
		return nil, errFuzzyScope
	}
	yes, err := fuzzyTable(ctx, r, table)
	if err != nil || !yes {
		return nil, err
	}
	rows, err := r.QueryContext(ctx, "SELECT * FROM "+table+" WHERE ID=?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) > 256 {
		return nil, errFuzzyScope
	}
	if !rows.Next() {
		return nil, rows.Err()
	}
	v, p := make([]any, len(columns)), make([]any, len(columns))
	for i := range p {
		p[i] = &v[i]
	}
	if err := rows.Scan(p...); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	for i, c := range columns {
		raw[strings.ToUpper(c)] = fuzzySQLCell(v[i])
	}
	if rows.Next() {
		return nil, errFuzzyScope
	}
	return raw, rows.Err()
}
func decodeFuzzyDocument(raw string, out any, limit int) error {
	if len(raw) > limit {
		return errFuzzyScope
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return errFuzzyScope
	}
	return nil
}
func fuzzySelector(r *http.Request) (string, int64, error) {
	kind := r.PathValue("type")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 || kind != "MOV" && kind != "TV" {
		return "", 0, errFuzzyScope
	}
	return kind, id, nil
}
func fuzzyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errFuzzyMissing) {
		writeAPIError(w, 404, 404, "fuzzy subscription or receipt not found")
		return
	}
	writeAPIError(w, 409, 409, "original fuzzy subscription scope or receipt could not be established; readonly review before any new action")
}
