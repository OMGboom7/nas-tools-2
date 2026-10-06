package httpserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"gopkg.in/yaml.v3"
)

type mediaCategoryAPI struct {
	config      *config.Store
	configPath  string
	defaultPath string
}

func (api mediaCategoryAPI) list(response http.ResponseWriter, request *http.Request) {
	if api.config == nil || api.configPath == "" {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "media configuration is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid category request")
		return
	}
	root, err := api.load()
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errCategoryName) {
			status = http.StatusBadRequest
		}
		writeAPIError(response, status, status, "media category configuration is invalid or unavailable")
		return
	}
	kind := "anime"
	switch request.Form.Get("type") {
	case "电影":
		kind = "movie"
	case "电视剧":
		kind = "tv"
	}
	items := categoryNames(root, kind)
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "category": items, "id": nil, "value": nil})
}

var errCategoryName = errors.New("invalid category configuration name")

func (api mediaCategoryAPI) load() (yaml.Node, error) {
	var root yaml.Node
	if api.config == nil || api.configPath == "" {
		return root, errors.New("media configuration is unavailable")
	}
	snapshot, err := api.config.Snapshot()
	if err != nil {
		return root, err
	}
	name := strings.TrimSpace(text(objectValue(snapshot["media"])["category"]))
	if name == "" {
		return root, nil
	}
	if name == "config" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return root, errCategoryName
	}
	contents, err := readCategoryTemplate(filepath.Join(filepath.Dir(api.configPath), name+".yaml"))
	if errors.Is(err, os.ErrNotExist) && api.defaultPath != "" {
		contents, err = readCategoryTemplate(api.defaultPath)
	}
	if err != nil {
		return root, err
	}
	if len(contents) > 1<<20 {
		return root, errors.New("category template too large")
	}
	if err := yaml.Unmarshal(contents, &root); err != nil {
		return root, err
	}
	return root, nil
}

func (api mediaCategoryAPI) match(kind string, info map[string]any) (string, error) {
	root, err := api.load()
	if err != nil {
		return "", err
	}
	return matchMediaCategory(root, kind, info)
}

// Conditions within a category are ANDed; comma-separated values within a
// condition are ORed. First match wins, including an unconditional fallback.
func matchMediaCategory(root yaml.Node, kind string, info map[string]any) (string, error) {
	if kind != "movie" && kind != "tv" && kind != "anime" {
		return "", errors.New("invalid category kind")
	}
	if len(info) == 0 || len(root.Content) == 0 {
		return "", nil
	}
	document := root.Content[0]
	if document.Kind != yaml.MappingNode {
		return "", errors.New("invalid category document")
	}
	var section *yaml.Node
	for offset := 0; offset+1 < len(document.Content); offset += 2 {
		if document.Content[offset].Value == kind {
			section = document.Content[offset+1]
			break
		}
	}
	if section == nil || section.Tag == "!!null" {
		return "", nil
	}
	section, err := categoryNode(section)
	if err != nil {
		return "", err
	}
	if section.Kind != yaml.MappingNode {
		return "", errors.New("invalid category section")
	}
	// TMDB details use genres, while searches and the old category matcher use
	// genre_ids. Normalize without changing the caller's response map.
	genres, hasGenres := info["genre_ids"]
	if !hasGenres {
		var ids []any
		for _, genre := range categoryArray(info["genres"]) {
			if object, ok := genre.(map[string]any); ok {
				ids = append(ids, object["id"])
			}
		}
		genres = ids
	}
	for offset := 0; offset+1 < len(section.Content); offset += 2 {
		name, conditions := section.Content[offset].Value, section.Content[offset+1]
		conditions, err := categoryNode(conditions)
		if err != nil {
			return "", err
		}
		if conditions.Tag == "!!null" || conditions.Kind == yaml.MappingNode && len(conditions.Content) == 0 {
			return name, nil
		}
		if conditions.Kind != yaml.MappingNode {
			continue
		}
		matched := true
		for attr := 0; attr+1 < len(conditions.Content); attr += 2 {
			key, rule := conditions.Content[attr].Value, conditions.Content[attr+1]
			rule, err := categoryNode(rule)
			if err != nil {
				return "", err
			}
			if rule.Tag == "!!null" || rule.Kind == yaml.ScalarNode && rule.Value == "" {
				continue
			}
			if rule.Kind != yaml.ScalarNode || rule.Tag != "!!str" {
				return "", errors.New("category condition must be a string")
			}
			value := info[key]
			if key == "genre_ids" {
				value = genres
			}
			values := categoryAttributeValues(key, value)
			intersects := false
			for _, expected := range strings.Split(rule.Value, ",") {
				if values[strings.ToUpper(expected)] {
					intersects = true
					break
				}
			}
			matched = matched && intersects
		}
		if matched {
			return name, nil
		}
	}
	return "", nil
}

func categoryNode(node *yaml.Node) (*yaml.Node, error) {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth >= 16 {
			return nil, errors.New("category alias depth exceeded")
		}
		node = node.Alias
	}
	if node == nil {
		return nil, errors.New("invalid category alias")
	}
	return node, nil
}

func categoryAttributeValues(key string, value any) map[string]bool {
	values := map[string]bool{}
	items := categoryArray(value)
	if items == nil {
		items = []any{value}
	}
	for _, item := range items {
		if key == "production_countries" {
			country, ok := item.(map[string]any)
			if !ok {
				continue
			}
			item = country["iso_3166_1"]
		}
		if item == nil || item == "" {
			continue
		}
		switch item.(type) {
		case string, float64, int, int64, bool:
			values[strings.ToUpper(fmt.Sprint(item))] = true
		}
	}
	return values
}

func categoryArray(value any) []any {
	switch items := value.(type) {
	case []any:
		return items
	case []string:
		result := make([]any, len(items))
		for index, item := range items {
			result[index] = item
		}
		return result
	case []int:
		result := make([]any, len(items))
		for index, item := range items {
			result[index] = item
		}
		return result
	case []map[string]any:
		result := make([]any, len(items))
		for index, item := range items {
			result[index] = item
		}
		return result
	}
	return nil
}

func readCategoryTemplate(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	return contents, err
}

func categoryNames(root yaml.Node, kind string) []string {
	items := []string{}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return items
	}
	for offset := 0; offset+1 < len(root.Content[0].Content); offset += 2 {
		if root.Content[0].Content[offset].Value != kind {
			continue
		}
		section := root.Content[0].Content[offset+1]
		if section.Kind != yaml.MappingNode {
			return items
		}
		for entry := 0; entry+1 < len(section.Content); entry += 2 {
			items = append(items, section.Content[entry].Value)
		}
		return items
	}
	return items
}
