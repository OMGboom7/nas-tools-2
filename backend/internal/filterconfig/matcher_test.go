package filterconfig

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuleMatchingPreservesLegacyConditions(t *testing.T) {
	upload, freeDownload, paidDownload := 2.0, 0.0, 1.0
	for _, test := range []struct {
		name     string
		rule     Rule
		metadata TorrentMetadata
		want     bool
	}{
		{"includes all lines", Rule{Include: "1080p\nWEB"}, TorrentMetadata{Title: "Example.1080p", Subtitle: "WEB-DL"}, true},
		{"missing include", Rule{Include: "1080p\nWEB"}, TorrentMetadata{Title: "Example.1080p.BluRay"}, false},
		{"excludes all lines", Rule{Exclude: "CAM\n720p"}, TorrentMetadata{Title: "Example CAM 720p"}, false},
		{"one exclusion absent", Rule{Exclude: "CAM\n720p"}, TorrentMetadata{Title: "Example CAM 1080p"}, true},
		{"exclusion alternation", Rule{Exclude: "CAM|TS"}, TorrentMetadata{Title: "Example TS"}, false},
		{"movie size lower boundary", Rule{Size: "1,10"}, TorrentMetadata{Movie: true, SizeBytes: 1 << 30}, true},
		{"movie size upper boundary", Rule{Size: "1,10"}, TorrentMetadata{Movie: true, SizeBytes: 10 * (1 << 30)}, true},
		{"movie size outside", Rule{Size: "1,10"}, TorrentMetadata{Movie: true, SizeBytes: 11 * (1 << 30)}, false},
		{"TV per episode", Rule{Size: "1,3"}, TorrentMetadata{Episodes: 10, SizeBytes: 20 * (1 << 30)}, true},
		{"TV per episode outside", Rule{Size: "1,3"}, TorrentMetadata{Episodes: 10, SizeBytes: 40 * (1 << 30)}, false},
		{"unknown episode count", Rule{Size: "1,3"}, TorrentMetadata{SizeBytes: 40 * (1 << 30)}, true},
		{"unknown size", Rule{Size: "1,3"}, TorrentMetadata{Movie: true}, true},
		{"promotion satisfied", Rule{Free: "1.0 0.0"}, TorrentMetadata{UploadFactor: &upload, DownloadFactor: &freeDownload}, true},
		{"promotion fails", Rule{Free: "1.0 0.0"}, TorrentMetadata{UploadFactor: &upload, DownloadFactor: &paidDownload}, false},
		{"unknown promotion", Rule{Free: "1.0 0.0"}, TorrentMetadata{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := MatchGroups([]GroupInfo{{Group: Group{ID: 1, Default: true}, Rules: []Rule{test.rule}}}, 0, test.metadata)
			if err != nil || result.Matched != test.want {
				t.Fatalf("matched=%v err=%v, want %v", result.Matched, err, test.want)
			}
		})
	}
}

func TestMatcherSelectsDefaultAndFirstMatchingPriority(t *testing.T) {
	groups := []GroupInfo{
		{Group: Group{ID: 1, Name: "Default", Default: true}, Rules: []Rule{{ID: 11, Priority: "1", Include: "2160p"}, {ID: 12, Priority: "2", Include: "1080p"}}},
		{Group: Group{ID: 2, Name: "Empty"}},
	}
	result, err := MatchGroups(groups, 0, TorrentMetadata{Title: "Example.1080p.WEB"})
	if err != nil || !result.Matched || result.Order != 98 || result.RuleID != 12 || result.GroupName != "Default" {
		t.Fatalf("default result=%+v err=%v", result, err)
	}
	result, err = MatchGroups(groups, 2, TorrentMetadata{})
	if err != nil || !result.Matched || result.Order != 0 {
		t.Fatalf("empty group result=%+v err=%v", result, err)
	}
	result, err = MatchGroups(groups, -1, TorrentMetadata{})
	if err != nil || !result.Matched || result.GroupName != "不过滤" {
		t.Fatalf("bypass result=%+v err=%v", result, err)
	}
	if _, err := MatchGroups(groups, 999, TorrentMetadata{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group err=%v", err)
	}
}

func TestBuiltinRulesCompileAndMatchKnownReleases(t *testing.T) {
	groups, err := Builtins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		for _, rule := range group.Rules {
			for _, raw := range []string{rule.Include, rule.Exclude} {
				for _, pattern := range strings.Split(raw, "\n") {
					if _, err := rulePatternMatches(pattern, ""); err != nil {
						t.Fatalf("builtin rule %d: %v", rule.ID, err)
					}
				}
			}
		}
	}
	result, err := MatchGroups(groups, 1000, TorrentMetadata{Title: "Example.2025.1080p.BluRay.x264", Subtitle: "特效字幕", Movie: true, SizeBytes: 10 * (1 << 30)})
	if err != nil || !result.Matched || result.RuleID != 10000 || result.Order != 99 {
		t.Fatalf("builtin release result=%+v err=%v", result, err)
	}
}

func TestMatcherReportsInvalidRulesInsteadOfAllowingTorrent(t *testing.T) {
	for _, rule := range []Rule{{Include: "[invalid"}, {Priority: "invalid"}, {Size: "1,2,3"}, {Free: "invalid"}} {
		factor := 1.0
		_, err := MatchGroups([]GroupInfo{{Group: Group{ID: 1}, Rules: []Rule{rule}}}, 1, TorrentMetadata{Title: "WEB1080p", Movie: true, SizeBytes: 2 * (1 << 30), UploadFactor: &factor, DownloadFactor: &factor})
		if !errors.Is(err, ErrInvalidRule) {
			t.Fatalf("rule=%+v err=%v", rule, err)
		}
	}
}

func TestStoreMatcherImmediatelySeesRuleChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	groupID, err := store.AddGroup(t.Context(), "Fresh", true)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{GroupID: groupID, Priority: "2", Include: "1080p"}
	rule.ID, err = store.SaveRule(t.Context(), rule)
	if err != nil {
		t.Fatal(err)
	}
	metadata := TorrentMetadata{Title: "Example.1080p"}
	result, err := store.Match(t.Context(), 0, metadata)
	if err != nil || !result.Matched {
		t.Fatalf("before update=%+v %v", result, err)
	}
	rule.Include = "2160p"
	if _, err := store.SaveRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}
	result, err = store.Match(t.Context(), 0, metadata)
	if err != nil || result.Matched {
		t.Fatalf("after update=%+v %v", result, err)
	}
}

func TestPythonRuleRegexCompatibility(t *testing.T) {
	for _, test := range []struct {
		pattern, text string
		want          bool
	}{
		{`(?<=WEB[.-])1080p`, "WEB.1080p", true},
		{`1080p(?!.*CAM)`, "1080p WEB", true},
		{`1080p(?!.*CAM)`, "1080p CAM", false},
		{`(WEB)[.-]\1`, "WEB-WEB", true},
		{`(?P<team>WEB)[.-](?P=team)`, "WEB.WEB", true},
		{`^\w+$`, "中文剧集", true},
		{`^\d+$`, "１２３", true},
		{`\b中文\b`, "中文", true},
		{`WEB\Z`, "WEB\n", false},
		{`WEB$`, "WEB\n", true},
		{`\(\?P<team>`, "(?P<team>", true},
	} {
		matched, err := rulePatternMatches(test.pattern, test.text)
		if err != nil || matched != test.want {
			t.Fatalf("pattern=%q text=%q: matched=%v err=%v want=%v", test.pattern, test.text, matched, err, test.want)
		}
	}
}

func TestRegexTimeoutDoesNotAcceptTorrent(t *testing.T) {
	_, err := rulePatternMatches(`^(a+)+$`, strings.Repeat("a", 4000)+"!")
	if !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("backtracking timeout err=%v", err)
	}
}
