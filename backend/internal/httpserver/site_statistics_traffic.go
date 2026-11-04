package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type statsTrafficRow struct {
	SiteID   string `json:"siteId"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Upload   string `json:"upload"`
	Download string `json:"download"`
}

func (api *siteStatisticsAPI) serveStatsTraffic(w http.ResponseWriter, r *http.Request) {
	if !api.readyForStatsRead(w) || !api.authorizeStatsRead(w, r) {
		return
	}
	query := r.URL.Query()
	days, _ := strconv.Atoi(query.Get("days"))
	switch days {
	case 1, 7, 30, 365:
	default:
		writeAPIError(w, 400, 400, "请选择一天、一周、一月或一年的统计范围")
		return
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		statsError(w, err)
		return
	}
	today := time.Now().In(loc).Format(time.DateOnly)
	endDay := query.Get("endDay")
	if endDay == "" {
		endDay = today
	}
	end, err := time.ParseInLocation(time.DateOnly, endDay, loc)
	if err != nil || end.Year() < 1 || end.Format(time.DateOnly) != endDay || endDay > today {
		writeAPIError(w, 400, 400, "统计日期无效")
		return
	}
	sites, err := api.readStatsSites(r.Context())
	if err != nil {
		statsError(w, err)
		return
	}
	byAddress := make(map[string]siteconfig.Site, len(sites))
	addresses := make([]string, 0, len(sites))
	for _, site := range sites {
		address := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
		if _, exists := byAddress[address]; !exists {
			byAddress[address] = site
			addresses = append(addresses, address)
		}
	}
	// One extra observation day supplies the original cumulative baseline.
	rows, err := api.store.Deltas(r.Context(), addresses, days+1, end)
	if err != nil {
		statsError(w, err)
		return
	}
	items := make([]statsTrafficRow, 0, len(rows))
	for _, row := range rows {
		site := byAddress[row.URL]
		items = append(items, statsTrafficRow{
			SiteID: strconv.FormatInt(site.ID, 10), Name: site.Name, Host: safeSiteHost(row.URL),
			Upload: strconv.FormatInt(row.Upload, 10), Download: strconv.FormatInt(row.Download, 10),
		})
	}
	writeJSON(w, 200, map[string]any{
		"code": 0, "success": true, "data": map[string]any{
			"days": days, "endDay": endDay, "today": today, "items": items,
		},
	})
}
