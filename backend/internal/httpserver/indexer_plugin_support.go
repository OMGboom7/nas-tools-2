package httpserver

// These providers either supply native indexers or have no indexer capability.
// Unknown plugins still prevent silently running a partial provider set.
func nativeSearchPlugin(id string) bool {
	switch id {
	case "Jackett", "Prowlarr", "CustomReleaseGroups", "Customization", "CustomHosts", "ChineseSubFinder", "LibraryScraper", "LibraryRefresh", "Webhook", "SpeedLimiter", "MediaSyncDel", "MovieLike", "OpenSubtitles":
		return true
	}
	return false
}
