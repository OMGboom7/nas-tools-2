package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestSelectorsHandleLegacyTablesAndQuotedHas(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseDocument(ctx, []byte(`<table class="torrents"><tr><th>header</th></tr><tr><td><table class="torrentname"><tr><td><a href="details.php?id=1">Movie</a></td></tr></table></td><td>8 GB</td></tr><tr><td>empty</td></tr></table>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{`table.torrents > tr:has("table.torrentname")`, `table.torrents > tbody > tr:nth-child(2)`, `table.torrents > tr:not(:has(th)):has(a[href*="details.php"])`, `table.torrents > tr:nth-last-child(2)`} {
		nodes, err := Select(ctx, doc.Root, selector)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("selector=%s count=%d error=%v", selector, len(nodes), err)
		}
		value, err := FieldValue(ctx, nodes[0], FieldSpec{Selector: `table.torrentname > tr > td > a`, Attribute: "href"})
		if err != nil || value != "details.php?id=1" {
			t.Fatal(value, err)
		}
	}
}

func TestFieldExtractionDoesNotMutateDocument(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseDocument(ctx, []byte(`<div class="name"> First <b>FREE</b><br>Second<br>Third</div><a href="download.php?id=1&amp;key=secret">one</a><a href="magnet:?xt=urn:btih:abc">two</a>`))
	if err != nil {
		t.Fatal(err)
	}
	last, second, missing := -1, 1, 99
	for _, fixture := range []struct {
		field FieldSpec
		want  string
	}{
		{FieldSpec{Selector: ".name", Remove: "b", Contents: &second}, "Second"},
		{FieldSpec{Selector: ".name", Remove: "b", Contents: &last}, "Third"},
		{FieldSpec{Selector: ".name"}, "First FREE\nSecond\nThird"},
		{FieldSpec{Selectors: "a", Attribute: "href"}, "download.php?id=1&key=secret"},
		{FieldSpec{Selector: "a", Attribute: "href", Index: &last}, "magnet:?xt=urn:btih:abc"},
		{FieldSpec{Selector: "a", Index: &missing}, ""},
		{FieldSpec{Selector: ".missing"}, ""},
	} {
		value, err := FieldValue(ctx, doc.Root, fixture.field)
		if err != nil || value != fixture.want {
			t.Fatalf("field=%+v value=%q expected=%q error=%v", fixture.field, value, fixture.want, err)
		}
	}
}

func TestSelectorAndDocumentLimits(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{"", "div[", `div:has("span"oops)`, strings.Repeat("table > tr ", 7), strings.Repeat("a", 8193)} {
		if _, err := compileSelector(raw); !errors.Is(err, ErrConfig) {
			t.Fatalf("expected configuration error: %q %v", raw, err)
		}
	}
	for _, body := range [][]byte{[]byte{0xff}, []byte(strings.Repeat("x", (4<<20)+1)), []byte(strings.Repeat("<div>", 130) + strings.Repeat("</div>", 130)), []byte(strings.Repeat("<i></i>", 50001))} {
		if _, err := ParseDocument(ctx, body); !errors.Is(err, ErrResponse) {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ParseDocument(cancelled, []byte("<p>test")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Select(cancelled, nil, "a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Select(ctx, nil, "a"); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	variants, err := selectorVariants(`a[data-rule="> tr"] > tr2`)
	if err != nil || len(variants) != 1 {
		t.Fatal(variants, err)
	}
}

func TestBundledSelectorRulesCompile(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	count, malformed := 0, 0
	for _, definition := range catalog.Indexers {
		var rules any
		if len(definition.Torrents) == 0 {
			continue
		}
		if err := json.Unmarshal(definition.Torrents, &rules); err != nil {
			t.Fatal(err)
		}
		var visit func(any)
		visit = func(value any) {
			switch value := value.(type) {
			case map[string]any:
				for key, item := range value {
					if key == "selector" || key == "selectors" || key == "remove" {
						if raw, ok := item.(string); ok && raw != "" {
							count++
							if _, err := compileSelector(raw); err != nil {
								// This existing rule is a template accidentally stored as CSS.
								// Reject it; do not count it as a supported selector.
								if definition.ID == "zimiao" && key == "selector" && strings.HasPrefix(raw, "{% if fields['tags']") {
									malformed++
								} else {
									t.Errorf("%s %s=%q: %v", definition.ID, key, raw, err)
								}
							}
						}
					}
					visit(item)
				}
			case []any:
				for _, item := range value {
					visit(item)
				}
			}
		}
		visit(rules)
	}
	if count != 1890 || malformed != 1 {
		t.Fatalf("unexpected selector coverage: total=%d malformed=%d", count, malformed)
	}
	t.Logf("compiled %d bundled selector rules; rejected %d malformed template rule", count-malformed, malformed)
}

func FuzzSelectorCompatibility(f *testing.F) {
	for _, raw := range []string{`table > tr:has("a")`, `a[href*="> tr"]`, `tr:not(:has(th))`, `a[`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) { _, _ = compileSelector(raw) })
}
