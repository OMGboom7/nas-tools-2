package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type storedPluginSetting struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Configured bool   `json:"configured"`
	Switch     *bool  `json:"switch,omitempty"`
}

type storedPluginSettings struct {
	Other  int                   `json:"other"`
	Kind   string                `json:"kind"`
	Fields []storedPluginSetting `json:"fields"`
}

func inspectStoredPluginSettings(raw, savedForm, digest string) (storedPluginSettings, error) {
	view := storedPluginSettings{Fields: []storedPluginSetting{}, Kind: "empty"}
	if raw == "" {
		return view, nil
	}
	if len(raw) > 4<<20 {
		return view, errors.New("stored plugin settings exceed limit")
	}
	// The old setting reader treated only strings beginning with { or [ as JSON.
	view.Kind = "text"
	if raw[0] != '{' && raw[0] != '[' {
		return view, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var data any
	if !json.Valid([]byte(raw)) || decoder.Decode(&data) != nil {
		return view, errors.New("invalid stored plugin settings")
	}
	object, ok := data.(map[string]any)
	if !ok {
		view.Kind = "list"
		return view, nil
	}
	view.Kind = "object"
	view.Other = len(object)
	var form struct {
		Digest, Revision string
		Keys             []string
	}
	hash := sha256.Sum256([]byte(raw))
	if digest == "" || len(savedForm) > 64<<10 || json.Unmarshal([]byte(savedForm), &form) != nil || form.Digest != digest || form.Revision != hex.EncodeToString(hash[:]) || len(form.Keys) > 300 {
		return view, nil
	}
	seen := map[string]bool{}
	for _, key := range form.Keys {
		value, present := object[key]
		if !present || seen[key] || cleanPluginKey(key) != key || key == "" {
			continue
		}
		seen[key] = true
		view.Other--
		field := storedPluginSetting{Key: key, Configured: configuredPluginValue(value)}
		switch v := value.(type) {
		case nil:
			field.Kind = "empty"
		case bool:
			field.Kind = "switch"
			field.Switch = &v
		case string:
			field.Kind = "text"
		case json.Number:
			field.Kind = "number"
		case map[string]any:
			field.Kind = "object"
			field.Configured = len(v) > 0
		case []any:
			field.Kind = "list"
			field.Configured = len(v) > 0
		}
		view.Fields = append(view.Fields, field)
	}
	sort.Slice(view.Fields, func(i, j int) bool { return view.Fields[i].Key < view.Fields[j].Key })
	return view, nil
}

func (s pluginService) serveStoredPluginConfig(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.system == nil || s.legacyURL != "" {
		return false
	}
	catalog, err := bundledPluginCatalog()
	if err != nil {
		writeAPIError(w, 500, 500, "插件目录不可用")
		return true
	}
	if _, bundled := catalog[id]; bundled {
		return false
	}
	admin, ok := s.catalogReader(w, r)
	if !ok {
		return true
	}
	if !admin || !systemconfig.ValidStoredPluginID(id) {
		writeAPIError(w, 404, 404, "未找到插件")
		return true
	}
	installed, raw, err := s.system.InstalledPluginConfig(r.Context(), id)
	if err != nil {
		writeAPIError(w, 503, 503, "无法读取已安装扩展的配置")
		return true
	}
	if !installed {
		writeAPIError(w, 404, 404, "未找到已安装插件")
		return true
	}
	declared, err := s.externalPluginCatalog(r.Context(), id)
	if err != nil {
		writeAPIError(w, 503, 503, "插件目录不可用")
		return true
	}
	name := id
	digest, form := "", ""
	if entry, found := declared[id]; found {
		if !entry.Visible(pluginReaderLevel(admin)) {
			writeAPIError(w, 404, 404, "未找到插件")
			return true
		}
		digest = entry.Digest
		form, err = s.system.Get(r.Context(), "GoExternalPluginFields."+id)
		if err != nil {
			writeAPIError(w, 503, 503, "无法读取扩展字段声明")
			return true
		}
		if label := cleanPluginText(entry.Fields["name"], 120); label != "" {
			name = label
		}
	}
	stored, err := inspectStoredPluginSettings(raw, form, digest)
	if err != nil {
		writeAPIError(w, 422, 422, "已保存的插件配置无法读取")
		return true
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": pluginConfigDetail{ID: id, Name: name, Fields: []pluginConfigField{}, Values: map[string]any{}, Stored: &stored}})
	return true
}
