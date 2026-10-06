package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestResolveFieldsCombinesSelectorsTemplatesAndFilters(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseDocument(ctx, []byte(`<div><a class="title" href="download.php?id=42">默认中文标题</a><span class="days">3</span><img class="free"></div>`))
	if err != nil {
		t.Fatal(err)
	}
	var rules map[string]FieldSpec
	if err := json.Unmarshal([]byte(`{
		"title_default":{"selector":".title"},
		"title_optional":{"selector":".optional","attribute":"title"},
		"title":{"text":"{% if fields['title_optional'] %}{{ fields['title_optional'] }}{% else %}{{ fields['title_default'][0:4] }}{% endif %}"},
		"download":{"selector":"a","attribute":"href","filters":[{"name":"re_search","args":["id=(\\d+)",1]}]},
		"hr_days":{"selector":".days"},
		"minimumseedtime":{"text":"{{ (fields['hr_days']|int)*86400 }}"},
		"downloadvolumefactor":{"case":{".free":0,"*":1}},
		"constant":{"text":0.8}
	}`), &rules); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		values, err := ResolveFields(ctx, doc.Root, rules)
		if err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]string{"title": "默认中文", "download": "42", "minimumseedtime": "259200", "downloadvolumefactor": "0", "constant": "0.8"} {
			if values[field] != want {
				t.Fatal(field, values[field], want)
			}
		}
	}
}

func TestResolveFieldsRejectsCyclesAndConflicts(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseDocument(ctx, []byte("<div></div>"))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"a":{"text":"{{ fields['a'] }}"}}`,
		`{"a":{"text":"{{ fields['b'] }}"},"b":{"text":"{{ fields['a'] }}"}}`,
		`{"a":{"text":"constant","selector":"a"}}`,
		`{"a":{"text":null}}`,
		`{"a":{"text":true}}`,
	} {
		var rules map[string]FieldSpec
		if err := json.Unmarshal([]byte(raw), &rules); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveFields(ctx, doc.Root, rules); !errors.Is(err, ErrConfig) {
			t.Fatal(raw, err)
		}
	}
}

func FuzzFieldTemplate(f *testing.F) {
	for _, raw := range []string{`{{ fields['title'] }}`, `{% if fields['x'] %}yes{% else %}no{% endif %}`, `{{ (fields['days']|int)*86400 }}`, `{{ fields['x"] }}`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		template, err := CompileFieldTemplate(raw)
		if err == nil {
			_, _ = template.Render(context.Background(), map[string]string{"title": "中文", "days": "3"})
		}
	})
}
