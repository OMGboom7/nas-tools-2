package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

func (service subscriptionService) nativeSubscriptionList(ctx context.Context) (subscriptionsData, error) {
	data := subscriptionsData{Items: []subscriptionItem{}, History: []subscriptionHistory{}, Warnings: []string{}}
	database, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return data, err
	}
	defer database.Close()
	for _, table := range []struct{ name, kind string }{{"RSS_MOVIES", "MOV"}, {"RSS_TVS", "TV"}} {
		rows, err := readSubscriptionRows(ctx, database, table.name)
		if err != nil {
			return data, err
		}
		items := make(map[string]any, len(rows))
		for _, raw := range rows {
			item := normalizeNativeSubscriptionRow(raw)
			items[text(item["id"])] = item
		}
		data.Items = append(data.Items, service.normalizeItems(items, table.kind)...)
	}
	historyRows, err := readSubscriptionRows(ctx, database, "RSS_HISTORY")
	if err != nil {
		return data, err
	}
	history := make([]any, 0, len(historyRows))
	for _, row := range historyRows {
		history = append(history, row)
	}
	data.History = service.normalizeHistory(history)
	sort.SliceStable(data.Items, func(left, right int) bool { return data.Items[left].Name < data.Items[right].Name })
	sort.SliceStable(data.History, func(left, right int) bool { return data.History[left].FinishTime > data.History[right].FinishTime })
	return data, nil
}

func (service subscriptionService) openNativeSubscriptionDatabase(ctx context.Context) (*sql.DB, error) {
	databaseURL := (&url.URL{Scheme: "file", Path: service.databasePath, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		return nil, err
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func readSubscriptionRows(ctx context.Context, database *sql.DB, table string) ([]map[string]any, error) {
	var exists int
	if err := database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return []map[string]any{}, nil
	}
	// table is selected only from the three fixed names supplied by nativeSubscriptionList.
	rows, err := database.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		scan := make([]any, len(columns))
		for index := range values {
			scan[index] = &values[index]
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for index, column := range columns {
			value := values[index]
			if bytes, ok := value.([]byte); ok {
				value = string(bytes)
			} else if integer, ok := value.(int64); ok {
				value = int(integer)
			}
			item[strings.ToUpper(column)] = value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func normalizeNativeSubscriptionRow(raw map[string]any) map[string]any {
	item := make(map[string]any, len(raw)+4)
	for key, value := range raw {
		item[strings.ToLower(key)] = value
	}
	description := text(raw["DESC"])
	item["overview"] = description
	for _, key := range []string{"rss_sites", "search_sites"} {
		item[key] = parseSubscriptionList(item[key])
	}
	if strings.Contains(description, "{") {
		var old map[string]any
		if json.Unmarshal([]byte(description), &old) == nil {
			item["overview"] = ""
			for key, target := range map[string]string{
				"rss_sites": "rss_sites", "search_sites": "search_sites", "over_edition": "over_edition",
				"restype": "filter_restype", "pix": "filter_pix", "team": "filter_team",
				"rule": "filter_rule", "include": "filter_include", "exclude": "filter_exclude",
				"total": "total_ep", "current": "current_ep",
			} {
				if value, ok := old[key]; ok {
					if key == "rss_sites" || key == "search_sites" {
						value = parseSubscriptionList(value)
					}
					if key == "over_edition" {
						value = text(value) == "Y"
					}
					item[target] = value
				}
			}
			item["save_path"] = ""
			item["download_setting"] = ""
			item["fuzzy_match"] = text(item["tmdbid"]) == ""
		}
	}
	var note map[string]any
	if json.Unmarshal([]byte(text(raw["NOTE"])), &note) == nil {
		item["poster"] = note["poster"]
	}
	return item
}

func parseSubscriptionList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case string:
		var values []any
		if json.Unmarshal([]byte(typed), &values) == nil {
			return values
		}
	}
	return []any{}
}
