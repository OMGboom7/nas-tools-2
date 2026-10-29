package movielike

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginalMovieRatingGateAndFields(t *testing.T) {
	for _, test := range []struct {
		raw            string
		handles, valid bool
	}{
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/Films/Movie/movie.mkv","Name":"Movie","Id":"one"}}`, true, true},
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"Z:\\Films\\Movie\\movie.mkv","Id":123}}`, true, true},
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/Films/Movie","UserData":{"IsFavorite":false}}}`, true, true},
		{`{"Event":"item.rate","Item":{"Type":"Episode","Path":"/emby/a.mkv"}}`, false, true},
		{`{"Event":"playback.start","Item":{"Type":"Movie","Path":"/emby/a.mkv"}}`, false, true},
		{`{"Event":"item.rate","Item":{"Type":"Movie"}}`, true, false},
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"https://evil.test/movie"}}`, true, false},
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/../movie"}}`, true, false},
		{`{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/movie","Name":42}}`, true, false},
	} {
		_, handles, err := Parse([]byte(test.raw))
		if handles != test.handles || (err == nil) != test.valid {
			t.Fatal(test.raw, handles, err)
		}
	}
	large := map[string]any{"Event": "item.rate", "Item": map[string]any{"Type": "Movie", "Path": "/emby/Movie"}, "unused": strings.Repeat("x", 70000)}
	raw, _ := json.Marshal(large)
	if _, handles, err := Parse(raw); err != nil || !handles {
		t.Fatal("receiver-valid callback was truncated to a settings budget", err)
	}
}
func TestThreeOriginalMappingsAreOrderedComponentBoundAndKeepUnknownIntegers(t *testing.T) {
	raw := `{"enable":true,"dir_name":"My Favorites","remote_path":"Z:/Films","local_path":"/stage","remote_path2":"/stage","local_path2":"/media","remote_path3":"/media","local_path3":"/library","unknown":{"n":9007199254740993}}`
	s, err := ParseSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{`Z:\Films\Movie\movie.mkv`: "/library/Movie/movie.mkv", "/media/Movie/media/movie.mkv": "/library/Movie/media/movie.mkv"} {
		got, err := s.Map(input)
		if err != nil || got != want {
			t.Fatal(input, got, want, err)
		}
	}
	if _, err := s.Map("Z:/Filmstrip/Movie/movie.mkv"); err == nil {
		t.Fatal("sibling prefix adopted as a mapping")
	}
	values, err := Values(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(values)
	if !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatal("unknown integer rounded")
	}
	root, err := ParseSpec(`{"dir_name":"精选","remote_path":"/","local_path":"/library"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := root.Map("/Films/Movie")
	if err != nil || got != "/library/Films/Movie" {
		t.Fatal(got, err)
	}
	for _, raw := range []string{`{"enable":"true"}`, `{"dir_name":"../escape"}`, `{"dir_name":".nastool-move-hidden"}`, `{"remote_path":"/a"}`, `{"remote_path":"/a","local_path":"relative"}`, `{"dir_name":"a/b"}`} {
		if _, err := ParseSpec(raw); err == nil {
			t.Fatal(raw)
		}
	}
}
