package builtinindexer

import (
	"encoding/json"
	"strings"
)

// The bundled HaiDan ID rule requests group 1 from a regex without a capture.
// Resolve its declared torrent_id query parameter explicitly, without rewriting
// the encoded catalog or changing unrelated user-provided filtering rules.
func normalizeHaiDanID(fields map[string]FieldSpec) {
	rule, ok := fields["id"]
	if !ok || rule.Attribute != "href" || !strings.Contains(rule.Selector, "torrent_id=") || len(rule.Filters) != 1 || rule.Filters[0].Name != "re_search" {
		return
	}
	var args []json.RawMessage
	if json.Unmarshal(rule.Filters[0].Args, &args) != nil || len(args) != 2 {
		return
	}
	var pattern string
	var group int
	if json.Unmarshal(args[0], &pattern) != nil || json.Unmarshal(args[1], &group) != nil || pattern != `\d+` || group != 1 {
		return
	}
	rule.Filters = []Filter{{Name: "querystring", Args: json.RawMessage(`"torrent_id"`)}}
	fields["id"] = rule
}
