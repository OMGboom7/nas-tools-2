package httpserver

// Jackett and Prowlarr supply the bundled plugin indexers. The other bundled
// plugins have no get_indexers implementation, so installing one does not
// change the providers used by a search. An unknown plugin may supply more.
func nativeSearchPlugin(id string) bool {
	catalog, err := bundledPluginCatalog()
	if err != nil {
		return false
	}
	_, known := catalog[id]
	return known
}
