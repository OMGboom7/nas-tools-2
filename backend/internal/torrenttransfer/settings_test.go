package torrenttransfer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTransferSettingsPreserveExactLegacyIDsAndUnknownJSON(t *testing.T) {
	raw := `{"enable":true,"fromdownloader":[9007199254740993],"todownloader":["9007199254740994"],"fromtorrentpath":"/fixture/torrents","opaque":{"large":9007199254740993,"null":null}}`
	settings, err := Parse(raw)
	if err != nil || settings.From[0] != "9007199254740993" || settings.To[0] != "9007199254740994" || settings.Ready() != nil {
		t.Fatal(settings, err)
	}
	updated, err := Merge(raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("true")})
	if err != nil || !strings.Contains(updated, `"large":9007199254740993`) {
		t.Fatal(updated, err)
	}
	for _, bad := range []string{`{"fromdownloader":[1.0]}`, `{"fromdownloader":["1","2"]}`, `{"todownloader":["01"]}`, `{"enable":"true"}`} {
		if _, err := Parse(bad); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := Merge(raw, map[string]json.RawMessage{"unreviewed": json.RawMessage("true")}); err == nil {
		t.Fatal("undeclared editor field accepted")
	}
}
func TestTransferDirectoryMappingUsesWholeComponentsAcrossRemotePlatforms(t *testing.T) {
	for _, tc := range []struct{ directory, from, to, want string }{
		{"/data/Film", "/data", "/library", "/library/Film"},
		{"/data2/Film", "/data", "/library", ""},
		{"/data/../private", "/data", "/library", ""},
		{`c:\data\Film`, `C:\data`, `D:\library`, "D:/library/Film"},
		{`\\server\share\Film`, `\\server\share`, "/library", "/library/Film"},
		{"/data/Film:Part", "/data", "/library", "/library/Film:Part"},
		{"/data/Film", "", "/library", "/data/Film"},
		{"", "/data", "/library", "/library"},
		{"/data/Film", "/", "/library", "/library/data/Film"},
	} {
		got, err := MapDirectory(tc.directory, tc.from, tc.to)
		if tc.want == "" {
			if err == nil {
				t.Fatal(tc, got)
			}
		} else if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}
func TestTransferExclusionsKeepLegacyDirectoryPrefixesAndTransmissionLabels(t *testing.T) {
	s := Settings{NoPaths: "/skip\n/also-skip/", NoLabels: "keep,source"}
	for _, tc := range []struct {
		directory string
		labels    []string
		want      bool
	}{
		{"/skip/Film", nil, true}, {"/skip-other/Film", []string{"keepers"}, true}, {"/data", []string{"keep"}, true}, {"/data", []string{"keep-too"}, false},
	} {
		got, err := s.Excluded("transmission", tc.directory, "", tc.labels)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}

func TestTransferQBExclusionsKeepSubstringAndEmptyEntryBehavior(t *testing.T) {
	s := Settings{NoLabels: "keep"}
	denied, err := s.Excluded("qbittorrent", "/data", "keepers", []string{"keepers"})
	if err != nil || !denied {
		t.Fatal(denied, err)
	}
	denied, err = s.Excluded("transmission", "/data", "", []string{"keepers"})
	if err != nil || denied {
		t.Fatal(denied, err)
	}
	s.NoLabels = ","
	denied, err = s.Excluded("qbittorrent", "/data", "any-label", []string{"any-label"})
	if err != nil || !denied {
		t.Fatal("old exclusion expanded", denied, err)
	}
	denied, err = s.Excluded("transmission", "/data", "", []string{"any-label"})
	if err != nil || denied {
		t.Fatal(denied, err)
	}
}
