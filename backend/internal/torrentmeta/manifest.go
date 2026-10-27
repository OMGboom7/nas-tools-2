package torrentmeta

import (
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

// Manifest validates the complete v1/v2/hybrid file tree. Parse remains the
// backwards-compatible identity reader for older nonselective workflows.
func Manifest(data []byte) (Metadata, []torrentfiles.File, error) {
	meta, err := Parse(data)
	if err != nil {
		return meta, nil, err
	}
	pos, nodes := 0, 0
	v, ok := manifestNode(data, &pos, 0, &nodes)
	if !ok || pos != len(data) {
		return meta, nil, ErrInvalid
	}
	root, ok := v.(map[string]any)
	if !ok {
		return meta, nil, ErrInvalid
	}
	info, ok := root["info"].(map[string]any)
	if !ok {
		return meta, nil, ErrInvalid
	}
	name := meta.Name
	if !component(name) {
		return meta, nil, ErrInvalid
	}
	files := []torrentfiles.File{}
	appendFile := func(name string, props map[string]any) bool {
		size, ok := props["length"].(int64)
		if !ok || size < 0 {
			return false
		}
		attrs, _ := props["attr"].(string)
		if strings.Contains(attrs, "l") || props["symlink path"] != nil {
			return false
		}
		files = append(files, torrentfiles.File{Index: len(files), Path: name, Size: size, Padding: strings.Contains(attrs, "p")})
		return len(files) <= torrentfiles.MaxFiles
	}
	if list, ok := info["files"].([]any); ok {
		if len(list) == 0 || info["length"] != nil {
			return meta, nil, ErrInvalid
		}
		for _, entry := range list {
			f, ok := entry.(map[string]any)
			if !ok {
				return meta, nil, ErrInvalid
			}
			raw, ok := f["path"].([]any)
			if utf := f["path.utf-8"]; utf != nil {
				raw, ok = utf.([]any)
			}
			if !ok || len(raw) == 0 || len(raw) > 64 {
				return meta, nil, ErrInvalid
			}
			parts := []string{name}
			for _, p := range raw {
				s, ok := p.(string)
				if !ok || !component(s) {
					return meta, nil, ErrInvalid
				}
				parts = append(parts, s)
			}
			if !appendFile(strings.Join(parts, "/"), f) {
				return meta, nil, ErrInvalid
			}
		}
	} else if info["files"] != nil {
		return meta, nil, ErrInvalid
	} else if _, ok := info["length"].(int64); ok {
		if !appendFile(name, info) {
			return meta, nil, ErrInvalid
		}
	}
	if version, has := info["meta version"]; has {
		if version != int64(2) {
			return meta, nil, ErrInvalid
		}
		tree, ok := info["file tree"].(map[string]any)
		if !ok || len(tree) == 0 || tree[""] != nil {
			return meta, nil, ErrInvalid
		}
		v1 := files
		files = []torrentfiles.File{}
		var walk func(map[string]any, []string) bool
		walk = func(tree map[string]any, parts []string) bool {
			keys := []string{}
			for k := range tree {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child, ok := tree[k].(map[string]any)
				if !ok {
					return false
				}
				if k == "" {
					if len(tree) != 1 || len(parts) == 0 {
						return false
					}
					if !appendFile(strings.Join(parts, "/"), child) {
						return false
					}
					continue
				}
				if !component(k) || !walk(child, append(append([]string{}, parts...), k)) {
					return false
				}
			}
			return true
		}
		if !walk(tree, nil) {
			return meta, nil, ErrInvalid
		}
		if len(files) > 1 {
			for i := range files {
				files[i].Path = name + "/" + files[i].Path
			}
		}
		if len(v1) > 0 {
			// Hybrid representations must agree, excluding v1 piece-alignment padding.
			real := map[string]int64{}
			for _, f := range v1 {
				if !f.Padding {
					real[f.Path] = f.Size
				}
			}
			for _, f := range files {
				if size, ok := real[f.Path]; !ok || size != f.Size {
					return meta, nil, ErrInvalid
				}
				delete(real, f.Path)
			}
			if len(real) > 0 {
				return meta, nil, ErrInvalid
			}
			files = v1
		}
	}
	files, err = torrentfiles.Normalize(files)
	if err != nil {
		return meta, nil, ErrInvalid
	}
	return meta, files, nil
}
func component(s string) bool { return !strings.Contains(s, "/") && torrentfiles.Relative(s) }
func manifestNode(data []byte, pos *int, depth int, nodes *int) (any, bool) {
	*nodes++
	if depth > 64 || *pos >= len(data) || *nodes > 150000 {
		return nil, false
	}
	switch data[*pos] {
	case 'd':
		*pos++
		m := map[string]any{}
		previous := ""
		for *pos < len(data) && data[*pos] != 'e' {
			k, ok := readString(data, pos)
			if !ok {
				return nil, false
			}
			key := string(k)
			if _, ok := m[key]; ok || len(m) > 0 && key < previous {
				return nil, false
			}
			previous = key
			v, ok := manifestNode(data, pos, depth+1, nodes)
			if !ok {
				return nil, false
			}
			m[key] = v
		}
		if *pos >= len(data) {
			return nil, false
		}
		*pos++
		return m, true
	case 'l':
		*pos++
		a := []any{}
		for *pos < len(data) && data[*pos] != 'e' {
			v, ok := manifestNode(data, pos, depth+1, nodes)
			if !ok {
				return nil, false
			}
			a = append(a, v)
		}
		if *pos >= len(data) {
			return nil, false
		}
		*pos++
		return a, true
	case 'i':
		start := *pos
		if !readValue(data, pos, depth) {
			return nil, false
		}
		raw := string(data[start+1 : *pos-1])
		if raw == "-0" {
			return nil, false
		}
		n, e := strconv.ParseInt(raw, 10, 64)
		return n, e == nil
	default:
		b, ok := readString(data, pos)
		return string(b), ok
	}
}
