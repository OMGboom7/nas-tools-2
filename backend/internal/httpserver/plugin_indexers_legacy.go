package httpserver

func indexerLegacyFields(detail pluginConfigDetail) []any {
	rows := []any{}
	for _, f := range detail.Fields {
		column := map[string]any{"title": f.Title, "tooltip": f.Tooltip, "type": f.Type, "configured": f.Configured}
		if f.Type == "switch" {
			column["id"] = f.Key
		} else if f.Type == "multiselect" {
			choices := map[string]any{}
			for _, choice := range f.Options {
				choices[choice.Value] = map[string]string{"name": choice.Label}
			}
			column["type"], column["id"], column["content"] = "form-selectgroup", f.Key, choices
		} else {
			field := map[string]any{"id": f.Key, "placeholder": f.Placeholder, "configured": f.Configured}
			if f.Type == "select" {
				choices := map[string]string{}
				for _, choice := range f.Options {
					choices[choice.Value] = choice.Label
				}
				field["options"] = choices
			}
			column["content"] = []any{field}
		}
		rows = append(rows, []any{column})
	}
	return []any{map[string]any{"type": "div", "content": rows}}
}
