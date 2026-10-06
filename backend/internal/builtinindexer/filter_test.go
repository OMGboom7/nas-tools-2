package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func decodeFilters(t *testing.T, raw string) []Filter {
	t.Helper()
	var filters []Filter
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		t.Fatal(err)
	}
	return filters
}

func TestFieldFilterOperations(t *testing.T) {
	for _, fixture := range []struct{ value, filters, want string }{
		{"  Title  ", `[{"name":"strip"}]`, "Title"},
		{"details.php?id=42", `[{"name":"re_search","args":["\\d+",0]}]`, "42"},
		{"id=４２", `[{"name":"re_search","args":["id=(\\d+)",1]}]`, "４２"},
		{"bad", `[{"name":"re_search","args":["tt\\d+",0]}]`, ""},
		{"first|second|last", `[{"name":"split","args":["|",-1]}]`, "last"},
		{"a.a", `[{"name":"replace","args":[".","/"]}]`, "a/a"},
		{"abc", `[{"name":"appendleft","args":"magnet:?xt=urn:btih:"}]`, "magnet:?xt=urn:btih:abc"},
		{"//path", `[{"name":"lstrip","args":["/"]}]`, "path"},
		{"?cat=movie%20HD&other=x", `[{"name":"replace","args":["?",""]},{"name":"querystring","args":"cat"}]`, "movie HD"},
		{"https://tracker.local/torrents.php?cat=1&cat=2#fragment", `[{"name":"querystring","args":"cat"}]`, "1"},
		{"2026年10月05日08点09分", `[{"name":"dateparse","args":"%Y年%m月%d日%H点%M分"}]`, "2026-10-05 08:09:00"},
		{"2026-10-0508:09:10", `[{"name":"dateparse","args":"%Y-%m-%d%H:%M:%S"}]`, "2026-10-05 08:09:10"},
		{"2026-10-05<br08:09:10", `[{"name":"dateparse","args":"%Y-%m-%d<br%H:%M:%S"}]`, "2026-10-05 08:09:10"},
	} {
		value, err := FilterValue(context.Background(), fixture.value, decodeFilters(t, fixture.filters))
		if err != nil || value != fixture.want {
			t.Fatalf("%q %s => %q error=%v; want=%q", fixture.value, fixture.filters, value, err, fixture.want)
		}
	}
	doc, err := ParseDocument(context.Background(), []byte(`<a href="download.php?id=42">download</a>`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := FieldValue(context.Background(), doc.Root, FieldSpec{Selector: "a", Attribute: "href", Filters: decodeFilters(t, `[{"name":"re_search","args":["id=(\\d+)",1]},{"name":"appendleft","args":"download/"}]`)})
	if err != nil || value != "download/42" {
		t.Fatal(value, err)
	}
}

func TestFieldFiltersRejectBadRulesAndResponses(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"re_search","args":["\\d+",1]}]`,
		`[{"name":"re_search","args":["[",0]}]`,
		`[{"name":"split","args":["",0]}]`,
		`[{"name":"replace","args":["x"]}]`,
		`[{"name":"querystring","args":null}]`,
		`[{"name":"lstrip","args":["/","?"]}]`,
		`[{"name":"strip","args":"x"}]`,
		`[{"name":"dateparse","args":"%"}]`,
	} {
		_, err := FilterValue(context.Background(), "", decodeFilters(t, raw))
		if !errors.Is(err, ErrConfig) {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{`[{"name":"shell"}]`, `[{"name":"dateparse","args":"%z"}]`} {
		_, err := FilterValue(context.Background(), "", decodeFilters(t, raw))
		if !errors.Is(err, ErrUnsupported) {
			t.Fatal(raw, err)
		}
	}
	for _, fixture := range []struct{ value, filters string }{
		{"one", `[{"name":"split","args":["|",2]}]`},
		{"cat=%invalid", `[{"name":"querystring","args":"cat"}]`},
		{"2026-02-30 08:00:00", `[{"name":"dateparse","args":"%Y-%m-%d %H:%M:%S"}]`},
		{strings.Repeat("a", 1<<20), `[{"name":"replace","args":["","0123456789"]}]`},
		{strings.Repeat("a", 64) + "!", `[{"name":"re_search","args":["^(a+)+$",0]}]`},
	} {
		_, err := FilterValue(context.Background(), fixture.value, decodeFilters(t, fixture.filters))
		if !errors.Is(err, ErrResponse) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FilterValue(ctx, "x", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := FilterValue(context.Background(), strings.Repeat("x", (4<<20)+1), nil); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
}

func TestBundledFilterCoverage(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	total, supported, unsupported, invalid := 0, 0, 0, 0
	for _, definition := range catalog.Indexers {
		var rules struct {
			Fields map[string]struct {
				Filters []Filter `json:"filters"`
			} `json:"fields"`
		}
		if len(definition.Torrents) == 0 {
			continue
		}
		if err := json.Unmarshal(definition.Torrents, &rules); err != nil {
			t.Fatal(err)
		}
		for _, field := range rules.Fields {
			for _, filter := range field.Filters {
				total++
				_, err := compileFilters([]Filter{filter})
				switch {
				case err == nil:
					supported++
				case errors.Is(err, ErrUnsupported):
					unsupported++
				case errors.Is(err, ErrConfig):
					invalid++
				default:
					t.Fatal(err)
				}
			}
		}
	}
	t.Logf("filters: total=%d supported=%d unsupported=%d invalid=%d", total, supported, unsupported, invalid)
	if total != 565 || unsupported != 0 || invalid != 1 || supported != 564 {
		t.Fatalf("unexpected filter inventory: %d %d %d %d", total, supported, unsupported, invalid)
	}
}
