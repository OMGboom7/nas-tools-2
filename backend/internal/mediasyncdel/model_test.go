package mediasyncdel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLegacyDeletionSelectorsVirtualFlagsAndPaths(t *testing.T) {
	for _, flag := range []string{`"False"`, `false`, `"false"`} {
		e, ok, err := Parse(json.RawMessage(`{"event_type":"media_del","item_isvirtual":` + flag + `,"media_type":"Episode","media_name":"Show","tmdb_id":"00100","media_path":"D:\\Media\\Show\\episode.mkv","season_num":"2","episode_num":3}`))
		if err != nil || !ok || e.TMDB != "100" || EpisodeKey(e) != "S02 E03" || e.Path != "D:/Media/Show/episode.mkv" {
			t.Fatal(e, ok, err)
		}
	}
	for _, flag := range []string{`"True"`, `true`, `"true"`} {
		e, ok, err := Parse(json.RawMessage(`{"event_type":"media_del","item_isvirtual":` + flag + `}`))
		if err != nil || !ok || !e.Virtual {
			t.Fatal(e, ok, err)
		}
	}
	for _, extra := range []string{"", `,"item_isvirtual":""`, `,"item_isvirtual":null`, `,"item_isvirtual":"unknown"`} {
		e, ok, err := Parse(json.RawMessage(`{"event_type":"media_del"` + extra + `}`))
		if err != nil || !ok || !e.MissingVirtual {
			t.Fatal(e, ok, err)
		}
	}
	if _, ok, err := Parse(json.RawMessage(`{"Event":"playback.start"}`)); err != nil || ok {
		t.Fatal(ok, err)
	}
	for _, field := range []string{`"media_type":"unknown"`, `"tmdb_id":"1;DROP TABLE x"`, `"media_path":"https://evil.test/a.mkv"`, `"media_path":"/a/../b"`} {
		raw := `{"event_type":"media_del","item_isvirtual":"False","media_type":"Movie","tmdb_id":"1","media_path":"/media/a.mkv",` + field + `}`
		// Receiver rejects duplicate keys; these unit fixtures exercise the final
		// invalid value as an independently parsed selector.
		if _, _, err := Parse(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid deletion selector", field)
		}
	}
}
func TestExclusionsUseWholeComponentsAndKeepOriginalUnknownValues(t *testing.T) {
	s, err := DecodeSpec(`{"enable":true,"del_source":true,"send_notify":true,"exclude_path":"/media/excluded/, D:\\Media\\Skip","extra":{"n":9007199254740993}}`)
	if err != nil || !s.Enabled || !s.DeleteSource || !s.Notify {
		t.Fatal(s, err)
	}
	for _, tc := range []struct {
		p    string
		want bool
	}{{"/media/excluded/a", true}, {"/media/excluded2/a", false}, {"D:/Media/Skip/a", true}, {"D:/Media/Skip2/a", false}} {
		if s.Excluded(tc.p) != tc.want {
			t.Fatal(tc)
		}
	}
	all, err := DecodeSpec(`{"exclude_path":"/"}`)
	if err != nil || !all.Excluded("/media/file") {
		t.Fatal(all, err)
	}
	for _, raw := range []string{`{"enable":"true"}`, `{"del_source":1}`, `{"exclude_path":"/media/../private"}`, `null`} {
		if _, err := DecodeSpec(raw); err == nil {
			t.Fatal(raw)
		}
	}
	a, _ := Values(`{"b":9007199254740993,"a":true}`)
	b, _ := Values(`{"a":true,"b":9007199254740993}`)
	if !reflect.DeepEqual(a, b) || Hash(a) != Hash(b) || Hash(a) == ConfigDigest(`{"b":9007199254740992,"a":true}`) {
		t.Fatal("unknown integer or key order lost")
	}
	if !SeasonMatches("S02 E01", 2) || SeasonMatches("S020 E01", 2) || SeasonMatches("S20 E01", 2) {
		t.Fatal("season prefix broadened selection")
	}
}
