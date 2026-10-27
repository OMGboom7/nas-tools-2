package torrentmeta

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func manifestEncode(v any) string {
	switch v := v.(type) {
	case string:
		return fmt.Sprintf("%d:%s", len(v), v)
	case int:
		return fmt.Sprintf("i%de", v)
	case []any:
		s := "l"
		for _, x := range v {
			s += manifestEncode(x)
		}
		return s + "e"
	case map[string]any:
		keys := []string{}
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := "d"
		for _, k := range keys {
			s += manifestEncode(k) + manifestEncode(v[k])
		}
		return s + "e"
	}
	panic("unexpected fixture type")
}
func manifestMetainfo(info map[string]any) []byte {
	return []byte(manifestEncode(map[string]any{"info": info}))
}
func TestManifestReadsFullV1ListWithUTF8NameAndDoesNotHashReencoding(t *testing.T) {
	info := map[string]any{"name": "Show.S01", "name.utf-8": "剧集.S01", "files": []any{map[string]any{"length": 12, "path": []any{"old.mkv"}, "path.utf-8": []any{"第2集.mkv"}}, map[string]any{"length": 0, "path": []any{".pad", "0"}, "attr": "p"}}}
	raw := manifestMetainfo(info)
	m, files, err := Manifest(raw)
	identity, _ := Parse(raw)
	if err != nil || m.InfoHash != identity.InfoHash || len(files) != 2 || files[0].Path != "剧集.S01/第2集.mkv" || !files[1].Padding {
		t.Fatal(m, files, err)
	}
}
func TestManifestReadsV2AndChecksHybridFileTreeAgreement(t *testing.T) {
	tree := map[string]any{"a.mkv": map[string]any{"": map[string]any{"length": 12}}, "b.mkv": map[string]any{"": map[string]any{"length": 13}}}
	info := map[string]any{"name": "Show", "meta version": 2, "file tree": tree}
	m, files, err := Manifest(manifestMetainfo(info))
	if err != nil || len(m.InfoHash) != 64 || len(files) != 2 || files[1].Path != "Show/b.mkv" {
		t.Fatal(m, files, err)
	}
	info["pieces"] = strings.Repeat("x", 20)
	info["files"] = []any{map[string]any{"length": 12, "path": []any{"a.mkv"}}, map[string]any{"length": 13, "path": []any{"b.mkv"}}}
	m, _, err = Manifest(manifestMetainfo(info))
	if err != nil || len(m.InfoHash) != 40 {
		t.Fatal(m, err)
	}
	info["files"] = []any{map[string]any{"length": 99, "path": []any{"a.mkv"}}, map[string]any{"length": 13, "path": []any{"b.mkv"}}}
	if _, _, err := Manifest(manifestMetainfo(info)); err == nil {
		t.Fatal("inconsistent hybrid accepted")
	}
}
func TestManifestRejectsMissingListsDuplicateKeysUnsafePathsAndSymlinks(t *testing.T) {
	bad := [][]byte{[]byte("d4:infod4:name1:ae"), []byte("d4:infod6:lengthi1e4:name1:a4:name1:bee")}
	for _, name := range []string{"../x", "/x", "a/b", "a\\b", "C:x", "."} {
		bad = append(bad, manifestMetainfo(map[string]any{"name": "Show", "files": []any{map[string]any{"length": 1, "path": []any{name}}}}))
	}
	bad = append(bad, manifestMetainfo(map[string]any{"name": "Show", "files": []any{map[string]any{"length": 1, "path": []any{"x"}, "attr": "l"}}}))
	bad = append(bad, manifestMetainfo(map[string]any{"name": "Show", "files": []any{map[string]any{"length": 1, "path": []any{"x"}}, map[string]any{"length": 1, "path": []any{"x"}}}}))
	bad = append(bad, manifestMetainfo(map[string]any{"name": "Show", "length": -1}))
	bad = append(bad, bytes.Repeat([]byte("d"), 65))
	for _, raw := range bad {
		if _, _, err := Manifest(raw); err == nil {
			t.Fatal("unsafe metainfo accepted", string(raw))
		}
	}
}
