package httpserver

// Only domain fields participate in public subscription events. Private rule
// caches, sender credentials and downloader configuration are not event data.
func subscriptionEventData(raw map[string]any, kind string, id int64, added bool) map[string]any {
	media := map[string]any{"type": kind}
	for key, column := range map[string]string{"title": "NAME", "year": "YEAR", "tmdb_id": "TMDBID", "season": "SEASON", "poster_path": "IMAGE"} {
		if value, ok := raw[column]; ok {
			media[key] = value
		}
	}
	out := map[string]any{"rssid": id}
	if added {
		out["media"] = media
		for key, column := range map[string]string{"rss_sites": "RSS_SITES", "search_sites": "SEARCH_SITES", "over_edition": "OVER_EDITION", "filter_restype": "FILTER_RESTYPE", "filter_pix": "FILTER_PIX", "filter_team": "FILTER_TEAM", "filter_rule": "FILTER_RULE", "save_path": "SAVE_PATH", "download_setting": "DOWNLOAD_SETTING", "total_ep": "TOTAL_EP"} {
			if value, ok := raw[column]; ok {
				out[key] = value
			}
		}
	} else {
		out["media_info"] = media
	}
	return out
}

func subscriptionDownloadEventData(raw map[string]any, kind string, id int64, title, url string, episodes []int) map[string]any {
	out := subscriptionEventData(raw, kind, id, false)
	out["torrent_file"] = url
	out["native_download"] = map[string]any{"resource_title": title, "episodes": episodes}
	if value, ok := raw["SAVE_PATH"]; ok {
		out["download_dir"] = value
	}
	if value, ok := raw["DOWNLOAD_SETTING"]; ok {
		out["download_setting"] = value
	}
	return out
}
