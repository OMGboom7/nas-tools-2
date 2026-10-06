package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type pluginConfigChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type pluginConfigField struct {
	Key         string               `json:"key"`
	Title       string               `json:"title"`
	Section     string               `json:"section"`
	Type        string               `json:"type"`
	Required    bool                 `json:"required"`
	Tooltip     string               `json:"tooltip"`
	Placeholder string               `json:"placeholder"`
	Options     []pluginConfigChoice `json:"options"`
	Default     any                  `json:"default"`
	WriteOnly   bool                 `json:"writeOnly"`
	Configured  bool                 `json:"configured"`
	ReadOnly    bool                 `json:"readOnly"`
}

type pluginConfigDetail struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Fields []pluginConfigField    `json:"fields"`
	Values map[string]any         `json:"values"`
	Meta   pluginConfigDetailMeta `json:"meta"`
}

type pluginConfigDetailMeta struct {
	HasPage bool `json:"hasPage"`
}

type pluginConfigRequest struct {
	Values      map[string]any `json:"values"`
	ClearConfig []string       `json:"clearConfig"`
}

func (service pluginService) serveConfig(response http.ResponseWriter, request *http.Request) {
	if service.nativeHosts(request.PathValue("id")) {
		service.serveHostsPlugin(response, request, "get")
		return
	}
	if service.system != nil && nativeMetadataPlugin(strings.TrimSpace(request.PathValue("id"))) {
		service.serveNativeMetadataConfig(response, request)
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid plugin id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	raw, result, err := service.rawInstalledPlugin(ctx, token, id)
	if !handlePluginResult(response, result, err, "plugin configuration is unavailable") {
		return
	}
	if raw == nil {
		writeAPIError(response, http.StatusNotFound, 404, "installed plugin not found")
		return
	}
	detail := safePluginConfigDetail(id, raw)
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": detail})
}

func (service pluginService) updateConfig(response http.ResponseWriter, request *http.Request) {
	if service.nativeHosts(request.PathValue("id")) {
		service.serveHostsPlugin(response, request, "save")
		return
	}
	if service.system != nil && nativeMetadataPlugin(strings.TrimSpace(request.PathValue("id"))) {
		service.serveNativeMetadataConfig(response, request)
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid plugin id")
		return
	}
	var input pluginConfigRequest
	if !decodeServiceRequest(response, request, &input, "invalid plugin configuration request") {
		return
	}
	if input.Values == nil {
		input.Values = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	raw, result, err := service.rawInstalledPlugin(ctx, token, id)
	if !handlePluginResult(response, result, err, "plugin configuration is unavailable") {
		return
	}
	if raw == nil {
		writeAPIError(response, http.StatusNotFound, 404, "installed plugin not found")
		return
	}
	detail := safePluginConfigDetail(id, raw)
	merged, message := mergePluginConfig(objectValue(raw["config"]), detail.Fields, input)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "plugin configuration is invalid")
		return
	}
	result, err = postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/config", token, url.Values{
		"id":     {id},
		"config": {string(encoded)},
	})
	if !handlePluginResult(response, result, err, "plugin configuration could not be saved") {
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "plugin configuration saved"})
}

func (service pluginService) rawInstalledPlugin(ctx context.Context, token, id string) (map[string]any, map[string]any, error) {
	result, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/list", token, nil)
	if err != nil || int(number(result["code"])) != 0 {
		return nil, result, err
	}
	raw, ok := objectValue(result["result"])[id]
	if !ok {
		return nil, result, nil
	}
	return objectValue(raw), result, nil
}

func safePluginConfigDetail(id string, raw map[string]any) pluginConfigDetail {
	current := objectValue(raw["config"])
	fields := normalizePluginConfigFields(raw["fields"], current)
	values := map[string]any{}
	for _, field := range fields {
		if field.WriteOnly || field.ReadOnly {
			continue
		}
		switch field.Type {
		case "switch":
			if value, exists := current[field.Key]; exists {
				values[field.Key] = truthy(value)
			} else {
				values[field.Key] = truthy(field.Default)
			}
		case "select":
			value := text(current[field.Key])
			if !pluginChoiceExists(field.Options, value) {
				value = text(field.Default)
			}
			if !pluginChoiceExists(field.Options, value) && len(field.Options) > 0 {
				value = field.Options[0].Value
			}
			values[field.Key] = value
		case "multiselect":
			values[field.Key] = safePluginSelection(current[field.Key], field.Options)
		}
	}
	name := cleanPluginText(raw["name"], 120)
	if name == "" {
		name = id
	}
	return pluginConfigDetail{
		ID: id, Name: name, Fields: fields, Values: values,
		Meta: pluginConfigDetailMeta{HasPage: cleanPluginText(raw["page"], 120) != ""},
	}
}

func normalizePluginConfigFields(raw any, current map[string]any) []pluginConfigField {
	fields := make([]pluginConfigField, 0)
	seen := map[string]bool{}
	for _, blockRaw := range sliceOrJSON(raw) {
		if len(fields) >= 300 {
			break
		}
		block := objectValue(blockRaw)
		typeName := strings.ToLower(text(block["type"]))
		if typeName != "div" && typeName != "details" {
			continue
		}
		section := ""
		if typeName == "details" {
			section = cleanPluginText(block["summary"], 100)
		}
		for _, rowRaw := range sliceOrJSON(block["content"]) {
			for _, columnRaw := range sliceOrJSON(rowRaw) {
				appendPluginColumnFields(&fields, seen, objectValue(columnRaw), section, current)
				if len(fields) >= 300 {
					return fields
				}
			}
		}
	}
	return fields
}

func appendPluginColumnFields(fields *[]pluginConfigField, seen map[string]bool, column map[string]any, section string, current map[string]any) {
	typeName := strings.ToLower(strings.TrimSpace(text(column["type"])))
	if typeName == "" {
		return
	}
	base := pluginConfigField{
		Title:    cleanPluginText(column["title"], 120),
		Section:  section,
		Required: truthy(column["required"]) || strings.EqualFold(text(column["required"]), "required"),
		Tooltip:  cleanPluginText(column["tooltip"], 800),
		ReadOnly: truthy(column["readonly"]),
		Options:  []pluginConfigChoice{},
	}
	switch typeName {
	case "switch":
		base.Key = cleanPluginKey(column["id"])
		base.Type = "switch"
		base.Default = truthy(column["default"])
		appendPluginField(fields, seen, base, current)
	case "textarea":
		content := objectValue(column["content"])
		base.Key = cleanPluginKey(content["id"])
		base.Type = "textarea"
		base.Placeholder = cleanPluginText(content["placeholder"], 240)
		base.WriteOnly = true
		appendPluginField(fields, seen, base, current)
	case "text", "password":
		entries := sliceOrJSON(column["content"])
		for index, entryRaw := range entries {
			entry := objectValue(entryRaw)
			field := base
			field.Key = cleanPluginKey(entry["id"])
			field.Type = "text"
			field.Placeholder = cleanPluginText(entry["placeholder"], 240)
			field.WriteOnly = true
			if index > 0 && field.Placeholder != "" {
				field.Title = field.Placeholder
			}
			appendPluginField(fields, seen, field, current)
		}
	case "select":
		for _, entryRaw := range sliceOrJSON(column["content"]) {
			entry := objectValue(entryRaw)
			field := base
			field.Key = cleanPluginKey(entry["id"])
			field.Type = "select"
			field.Options = pluginChoices(entry["options"])
			candidate := cleanPluginText(entry["default"], 200)
			if pluginChoiceExists(field.Options, candidate) {
				field.Default = candidate
			}
			appendPluginField(fields, seen, field, current)
		}
	case "form-selectgroup":
		base.Key = cleanPluginKey(column["id"])
		base.Type = "multiselect"
		base.Options = pluginChoices(column["content"])
		appendPluginField(fields, seen, base, current)
	}
}

func appendPluginField(fields *[]pluginConfigField, seen map[string]bool, field pluginConfigField, current map[string]any) {
	if field.Key == "" || seen[field.Key] || len(*fields) >= 300 {
		return
	}
	if field.Title == "" {
		field.Title = field.Placeholder
	}
	if field.Title == "" {
		field.Title = field.Key
	}
	field.Configured = configuredPluginValue(current[field.Key])
	seen[field.Key] = true
	*fields = append(*fields, field)
}

func cleanPluginKey(value any) string {
	key := strings.TrimSpace(text(value))
	if key == "" || len(key) > 120 {
		return ""
	}
	for _, character := range key {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' && character != '.' {
			return ""
		}
	}
	return key
}

func pluginChoices(raw any) []pluginConfigChoice {
	choices := make([]pluginConfigChoice, 0)
	for value, labelRaw := range objectValue(raw) {
		if len(choices) >= 200 {
			break
		}
		value = cleanPluginText(value, 200)
		if value == "" {
			continue
		}
		labelObject := objectValue(labelRaw)
		label := cleanPluginText(labelObject["name"], 200)
		if label == "" {
			label = cleanPluginText(labelRaw, 200)
		}
		if label == "" {
			label = value
		}
		choices = append(choices, pluginConfigChoice{Value: value, Label: label})
	}
	sort.Slice(choices, func(left, right int) bool { return choices[left].Label < choices[right].Label })
	return choices
}

func pluginChoiceExists(options []pluginConfigChoice, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func safePluginSelection(raw any, options []pluginConfigChoice) []string {
	selected := make([]string, 0)
	seen := map[string]bool{}
	for _, value := range sliceOrJSON(raw) {
		item := text(value)
		if pluginChoiceExists(options, item) && !seen[item] {
			seen[item] = true
			selected = append(selected, item)
		}
	}
	return selected
}

func configuredPluginValue(value any) bool {
	switch item := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(item) != ""
	case []any:
		return len(item) > 0
	default:
		return true
	}
}

func mergePluginConfig(current map[string]any, fields []pluginConfigField, input pluginConfigRequest) (map[string]any, string) {
	merged := make(map[string]any, len(current)+len(fields))
	for key, value := range current {
		merged[key] = value
	}
	byKey := make(map[string]pluginConfigField, len(fields))
	for _, field := range fields {
		byKey[field.Key] = field
	}
	for key := range input.Values {
		if _, ok := byKey[key]; !ok {
			return nil, "unknown plugin configuration field"
		}
	}
	clear := map[string]bool{}
	for _, key := range input.ClearConfig {
		field, ok := byKey[key]
		if !ok || !field.WriteOnly || field.ReadOnly {
			return nil, "plugin configuration field cannot be cleared"
		}
		clear[key] = true
	}
	for key, raw := range input.Values {
		field := byKey[key]
		if field.ReadOnly {
			return nil, "read-only plugin configuration field cannot be changed"
		}
		switch field.Type {
		case "text", "textarea":
			value, ok := raw.(string)
			if !ok || len([]rune(value)) > 20000 || strings.ContainsRune(value, '\x00') {
				return nil, "plugin text configuration is invalid"
			}
			if value != "" {
				merged[key] = value
			}
		case "switch":
			value, ok := raw.(bool)
			if !ok {
				return nil, "plugin switch configuration is invalid"
			}
			merged[key] = value
		case "select":
			value, ok := raw.(string)
			if !ok || !pluginChoiceExists(field.Options, value) {
				return nil, "plugin select configuration is invalid"
			}
			merged[key] = value
		case "multiselect":
			values, ok := raw.([]any)
			if !ok || len(values) > len(field.Options) {
				return nil, "plugin multi-select configuration is invalid"
			}
			selected := make([]string, 0, len(values))
			seen := map[string]bool{}
			for _, rawValue := range values {
				value, ok := rawValue.(string)
				if !ok || !pluginChoiceExists(field.Options, value) || seen[value] {
					return nil, "plugin multi-select configuration is invalid"
				}
				seen[value] = true
				selected = append(selected, value)
			}
			merged[key] = selected
		}
	}
	for key := range clear {
		delete(merged, key)
	}
	for _, field := range fields {
		if !field.Required || field.ReadOnly || field.Type == "switch" {
			continue
		}
		if !configuredPluginValue(merged[field.Key]) {
			return nil, "required plugin configuration is missing: " + field.Title
		}
	}
	return merged, ""
}
