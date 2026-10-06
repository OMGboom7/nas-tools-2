package httpserver

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLibraryStringFormatting(t *testing.T) {
	values := map[string]string{"title": "你好World", "season": "1", "width": "4", "missing": ""}
	for _, test := range []struct{ template, want string }{
		{"{season:0>2}", "01"}, {"{season:02}", "10"}, {"{season:>3}", "  1"}, {"{season: >03}", "  1"},
		{"{season:*^4}", "*1**"}, {"{season:字<3}", "1字字"}, {"{title:.2s}", "你好"},
		{"{title:>5.2}", "   你好"}, {"{title!s:.3}", "你好W"}, {"{{{season}}}", "{1}"},
		{"{season:0>{width}}", "0001"}, {"{season:0>{width:1}}", "0001"},
		{"A - {missing}B", "A - \tB"}, {"{title:.0}", ""},
	} {
		got, err := renderLocalTemplate(test.template, values, 0)
		if err != nil || got != test.want {
			t.Errorf("%q=%q want=%q err=%v", test.template, got, test.want, err)
		}
	}
	for _, template := range []string{"{season:02d}", "{title!r}", "{title!a}", "{title:=8}", "{title:+5}", "{title:,}", "{title:.}", "{title:.s}", "{title:4097}", "{title:9999999999999999999}", "{title:0>{width:{season}}}", "{title.__class__}", "{unknown}", "{title", "title}", "{}"} {
		if got, err := renderLocalTemplate(template, values, 0); err == nil {
			t.Errorf("unsupported %q returned %q", template, got)
		}
	}
	for _, template := range []string{"{title:4096}{title}", "{title:字>4096}", strings.Repeat("x", 4097), strings.Repeat("{", 16385)} {
		if _, err := renderLocalTemplate(template, values, 0); err == nil {
			t.Errorf("oversized format accepted")
		}
	}
}

func FuzzLibraryDirectoryFormatting(f *testing.F) {
	for _, template := range []string{"{title}/Season {season:0>2}", "{title:.2}", "{title:0>{width}}", "{{{title}}}", "{title.__class__}", "../{title}"} {
		f.Add(template)
	}
	f.Fuzz(func(t *testing.T, template string) {
		result, err := renderLocalDirectory(template, map[string]string{"title": "..你好世界", "season": "1", "width": "8"})
		if err != nil {
			return
		}
		if len(result) > 4096 || result == "" || filepath.IsAbs(result) || strings.ContainsAny(result, "\\\x00") {
			t.Fatalf("unsafe rendered directory %q", result)
		}
		for _, part := range strings.Split(result, "/") {
			if part == "" || part == "." || part == ".." {
				t.Fatalf("unsafe rendered component %q", result)
			}
		}
	})
}

func TestFormattedLibraryDirectoriesStillRejectTraversal(t *testing.T) {
	for _, template := range []string{"{title:.2}/Movie", "{title:/>4.1}", "/{season:0>2}"} {
		if got, err := renderLocalDirectory(template, map[string]string{"title": "..escape", "season": "1"}); err == nil {
			t.Fatalf("unsafe formatted path %q=%q", template, got)
		}
	}
	got, err := renderLocalDirectory("{title:.2}/Season {season:0>2}", map[string]string{"title": "你好世界", "season": "1"})
	if err != nil || got != "你好/Season 01" {
		t.Fatalf("Unicode directory=%q %v", got, err)
	}
}
