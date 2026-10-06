package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRelativeDatesUseExplicitClockAndDirection(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 9, 10, 0, time.FixedZone("CST", 8*3600))
	for _, fixture := range []struct {
		value   string
		english bool
		want    string
	}{
		{"2日3時4分", false, "2026-10-07 11:13:10"},
		{"1天2小时3分钟4秒前", false, "2026-10-04 06:06:06"},
		{"1週後", false, "2026-10-12 08:09:10"},
		{"刚刚", false, "2026-10-05 08:09:10"},
		{"2 days 3 hours ago", true, "2026-10-03 05:09:10"},
		{"1 week, 2 days and 3 mins ago", true, "2026-09-26 08:06:10"},
		{"0 seconds", true, "2026-10-05 08:09:10"},
		{"just now", true, "2026-10-05 08:09:10"},
	} {
		value, err := relativeDate(fixture.value, now, fixture.english)
		if err != nil || value != fixture.want {
			t.Fatal(fixture, value, err)
		}
	}
	for _, raw := range []string{"", "abc 2 days", "-1 day", "1day garbage", "1 month", "999999999999 days", "1 day 2 days", ",1 day", "2 days from now"} {
		if _, err := relativeDate(raw, now, true); !errors.Is(err, ErrResponse) {
			t.Fatal(raw, err)
		}
	}
	if _, err := relativeDate("1日", time.Time{}, false); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
}

func TestDateFiltersAndFieldsShareFixedClock(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 8, 9, 10, 0, time.UTC)
	value, err := FilterValueAt(ctx, "now", decodeFilters(t, `[{"name":"dateparse","args":"%Y-%m-%d %H:%M:%S"}]`), now)
	if err != nil || value != "2026-10-05 08:09:10" {
		t.Fatal(value, err)
	}
	value, err = FilterValueAt(ctx, "限時：2日3時4分", decodeFilters(t, `[{"name":"re_search","args":["(?:限時：\\s*)((?:\\d+日)?(?:\\d+時)?(?:\\d+分)?)",1]},{"name":"date_elapsed_parse"}]`), now)
	if err != nil || value != "2026-10-07 11:13:10" {
		t.Fatal(value, err)
	}
	doc, err := ParseDocument(ctx, []byte(`<span class="elapsed">2 days ago</span><span class="remaining">1日</span>`))
	if err != nil {
		t.Fatal(err)
	}
	var rules map[string]FieldSpec
	if err := json.Unmarshal([]byte(`{
		"date_elapsed":{"selector":".elapsed","filters":[{"name":"date_en_elapsed_parse"}]},
		"free_deadline":{"selector":".remaining","filters":[{"name":"date_elapsed_parse"}]},
		"date":{"text":"{% if fields['date_elapsed'] or fields['date_added'] %}{{ fields['date_elapsed'] if fields['date_elapsed'] else fields['date_added'] }}{% else %}now{% endif %}","filters":[{"name":"dateparse","args":"%Y-%m-%d %H:%M:%S"}]}
	}`), &rules); err != nil {
		t.Fatal(err)
	}
	values, err := ResolveFieldsAt(ctx, doc.Root, rules, now)
	if err != nil {
		t.Fatal(err)
	}
	if values["date"] != "2026-10-03 08:09:10" || values["free_deadline"] != "2026-10-06 08:09:10" {
		t.Fatal(values)
	}
	delete(rules, "date_elapsed")
	values, err = ResolveFieldsAt(ctx, doc.Root, rules, now)
	if err != nil || values["date"] != "2026-10-05 08:09:10" {
		t.Fatal(values, err)
	}
	if _, err := ResolveFieldsAt(ctx, doc.Root, rules, time.Time{}); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	if _, err := compileFilters([]Filter{{Name: "date_elapsed_parse", Args: json.RawMessage(`"unexpected"`)}}); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
}
