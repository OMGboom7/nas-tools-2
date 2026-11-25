package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestDownloadLayoutRetainsNestedNamesAndSkipsPadding(t *testing.T) {
	file := func(path []any, attr string) any {
		return map[string]any{"path": path, "length": 8, "attr": attr}
	}
	for _, tt := range []struct {
		info   map[string]any
		folder string
		names  []string
	}{
		{map[string]any{"name": "movie.mkv", "length": 8}, "", []string{"movie.mkv"}},
		{map[string]any{"name": "Show", "files": []any{file([]any{"S01", "E01.mkv"}, ""), file([]any{".pad", "8"}, "p")}}, "Show", []string{"S01/E01.mkv"}},
		{map[string]any{"name": "Show", "meta version": 2, "file tree": map[string]any{"Show": map[string]any{"E02.mkv": map[string]any{"": map[string]any{"length": 8}}}}}, "Show", []string{"Show/E02.mkv"}},
		{map[string]any{"name": "Show", "files": []any{file([]any{".pad", "8"}, "p")}}, "", nil},
	} {
		folder, files, err := DownloadLayout(manifestMetainfo(tt.info))
		if (tt.names == nil) != (err != nil) || folder != tt.folder || !slices.Equal(files, tt.names) {
			t.Fatal(folder, files, err)
		}
	}
}

func TestMagnetTopicsHashOriginalInfoAndKeepDistinctTrackerURLs(t *testing.T) {
	tree := map[string]any{"Film & 电影": map[string]any{"": map[string]any{"length": 8}}}
	for _, hybrid := range []bool{false, true} {
		info := map[string]any{"name": "Film & 电影", "meta version": 2, "file tree": tree}
		if hybrid {
			info["pieces"] = strings.Repeat("x", 20)
			info["length"] = 8
		}
		trackers := []string{"https://tracker.fixture/a?k=test&b=1", "udp://tracker.fixture:80/a"}
		data := []byte(manifestEncode(map[string]any{"announce": trackers[0], "announce-list": []any{[]any{trackers[0], trackers[1]}}, "info": info}))
		rawInfo := mustTransferInfo(data)
		v2 := sha256.Sum256(rawInfo)
		want := []string{"urn:btmh:1220" + hex.EncodeToString(v2[:])}
		if hybrid {
			v1 := sha1.Sum(rawInfo)
			want = append([]string{"urn:btih:" + hex.EncodeToString(v1[:])}, want...)
		}
		link, err := MagnetLink(data)
		u, parseErr := url.Parse(link)
		if err != nil || parseErr != nil || !slices.Equal(u.Query()["xt"], want) || u.Query().Get("dn") != "Film & 电影" || !slices.Equal(u.Query()["tr"], trackers) {
			t.Fatal("wrong magnet identity, title or trackers", err, parseErr)
		}
	}
}

func TestResolveLinkVerifiesAdvertisedHashes(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NASTOOL_ARIA2_BINARY", binary)
	id := sha1.Sum(mustTransferInfo(metadataHelperRaw))
	link := "magnet:?xt=urn:btih:" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:]) + "&tr=http%3A%2F%2Fprivate-tracker"
	raw, err := ResolveLink(t.Context(), link)
	if err != nil || !bytes.Equal(raw, metadataHelperRaw) {
		t.Fatal(err)
	}
	for _, suffix := range []string{"&xt=urn:btmh:1220" + strings.Repeat("a", 64), "&xt=urn:btih:" + strings.Repeat("0", 40), "\n invalid"} {
		if _, err := ResolveLink(t.Context(), link+suffix); err == nil {
			t.Fatal("conflicting or invalid magnet accepted")
		}
	}
}
