package filterconfig

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceDecision(t *testing.T) {
	free, one := 0., 1.
	q := resourceFilter{Quality: "WEB", Edition: "WEB-DL", Resolution: "4k", Pixels: "2160p", Team: "GROUP|译组", Separator: "/", Include: "(?<=WEB-)DL", Exclude: "CAM", Keyword: "字幕", Promotion: "* 0.0", Download: &free,
		Metadata: TorrentMetadata{Title: "剧.WEB-DL-GROUP@译组@GROUP", Subtitle: "字幕", SizeBytes: 4 << 30, Episodes: 2, RequireKnown: true}}
	groups := []GroupInfo{{Group: Group{ID: 8, Default: true}, Rules: []Rule{{ID: 9, Priority: "2", Size: "1,3"}}}}
	for _, tt := range []struct {
		name    string
		edit    func(*resourceFilter)
		matched bool
		reason  string
		team    string
		size    bool
	}{
		{"full decision", func(*resourceFilter) {}, true, "", "GROUP/译组", true},
		{"preclassified", func(q *resourceFilter) { q.ReleaseGroup = "group" }, true, "", "", true},
		{"capture", func(q *resourceFilter) { q.Team = "G(ROUP)" }, true, "", "ROUP", true},
		{"missing edition", func(q *resourceFilter) { q.Edition = "" }, false, "质量", "", false},
		{"resolution", func(q *resourceFilter) { q.Pixels = "720p" }, false, "分辨率", "", false},
		{"paid", func(q *resourceFilter) { q.Download = &one; q.Include = "absent" }, false, "促销", "GROUP/译组", false},
		{"zero cannot pass paid", func(q *resourceFilter) { q.Promotion = "* 1" }, false, "促销", "GROUP/译组", false},
		{"unknown promotion", func(q *resourceFilter) { q.Download = nil }, false, "促销", "GROUP/译组", false},
		{"text condition", func(q *resourceFilter) { q.Exclude = "字幕" }, false, "排除", "GROUP/译组", false},
		{"unknown count", func(q *resourceFilter) { q.Metadata.Episodes = 0 }, false, "", "GROUP/译组", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := q
			tt.edit(&input)
			got, err := input.evaluate(t.Context(), groups)
			if err != nil || got.Matched != tt.matched || !strings.Contains(got.Reason, tt.reason) || got.Team != tt.team || got.SizeChecked != tt.size || got.Matched && got.Order != 98 {
				t.Fatalf("%+v, %v", got, err)
			}
		})
	}
}

func TestResourceSearchScope(t *testing.T) {
	s := releaseScope{Seasons: []int{2}, WantedSeasons: []int{2}, Episodes: []int{1, 4}, WantedEpisodes: []int{1, 4}, Year: "2026", WantedYear: "2026"}
	if !s.matches() {
		t.Fatal("non-contiguous selection rejected")
	}
	s.Episodes = []int{1, 2, 4}
	if s.matches() {
		t.Fatal("partial overlap accepted")
	}
	s.Episodes = nil
	if !s.matches() {
		t.Fatal("whole season excluded")
	}
	s.WantedYear = "2025"
	if s.matches() {
		t.Fatal("incorrect year")
	}
	s.WantedYear = ""
	s.Seasons = nil
	if s.matches() {
		t.Fatal("unknown season")
	}
}

func TestResourceCommandSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	db := filepath.Join(root, "user.db")
	if err := os.WriteFile(db, nil, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWritable(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.AddGroup(t.Context(), "example", true)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{GroupID: id, Priority: "3", Include: "example", Free: "1 0"}
	rule.ID, err = store.SaveRule(t.Context(), rule)
	if err != nil {
		t.Fatal(err)
	}
	in, write := io.Pipe()
	out, read := io.Pipe()
	defer write.Close()
	defer out.Close()
	done := make(chan error, 1)
	go func() { err := ServeResources(t.Context(), path, in, read); in.Close(); read.Close(); done <- err }()
	decoder := json.NewDecoder(out)
	call := func(line string, result any) {
		t.Helper()
		if _, err := io.WriteString(write, line+"\n"); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(result); err != nil {
			t.Fatal(err)
		}
	}
	q := `{"op":"match","filter":{"metadata":{"title":"example","requireKnown":true,"uploadFactor":1,"downloadFactor":0}}}`
	var first resourceDecision
	call(q, &first)
	if !first.Matched || first.Order != 97 {
		t.Fatal(first)
	}
	rule.Include = "other"
	if _, err := store.SaveRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}
	var highest int
	call(`{"op":"highest"}`, &highest)
	if highest != 97 {
		t.Fatal(highest)
	}
	var needs bool
	call(`{"op":"needsPromotion"}`, &needs)
	if !needs {
		t.Fatal("missing promotion check")
	}
	var invalid map[string]bool
	call(`{"op":"match","filter":{"group":599}}`, &invalid)
	if !invalid["error"] {
		t.Fatal(invalid)
	}
	var second resourceDecision
	call(q, &second)
	if second != first {
		t.Fatal(second)
	}
	write.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var reply bytes.Buffer
	if err := ServeResources(t.Context(), path, strings.NewReader(q), &reply); err != nil {
		t.Fatal(err)
	}
	var latest resourceDecision
	if err := json.Unmarshal(reply.Bytes(), &latest); err != nil || latest.Matched || latest.GroupName != "example" {
		t.Fatal(latest, err)
	}
}

func TestInvalidResourceFrames(t *testing.T) {
	for _, frame := range []string{`{"op":"scope"}{}`, `{"op":"match","unknown":true}`, strings.Repeat("z", 513<<10)} {
		var out bytes.Buffer
		if err := ServeResources(t.Context(), "", strings.NewReader(frame), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid frame produced a decision")
		}
	}
}
