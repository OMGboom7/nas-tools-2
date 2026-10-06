package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestFieldTemplates(t *testing.T) {
	fields := map[string]string{"title_default": "默认中文片名", "title_optional": "", "subject": "标题", "tags": "标签", "hr_days": "3", "zero": "0"}
	for _, fixture := range []struct{ raw, want string }{
		{`{% if fields['title_optional'] %}{{ fields['title_optional'] }}{% else %}{{ fields['title_default'] }}{% endif %}`, "默认中文片名"},
		{`{{ fields['title_default'][0:2] }}`, "默认"},
		{`{{ fields['subject']+' '+fields['tags'] }}`, "标题 标签"},
		{`{% if fields['hr_days'] %}{{ (fields['hr_days']|int)*86400 }}{% else %}0{% endif %}`, "259200"},
		{`{% if fields['zero'] %}yes{% else %}no{% endif %}`, "yes"},
		{`{{ fields['zero'] or fields['subject'] }}`, "0"},
		{`{% if 0 %}yes{% else %}no{% endif %}`, "no"},
		{`{% if fields['date_elapsed'] or fields['date_added'] %}{{ fields['date_elapsed'] if fields['date_elapsed'] else fields['date_added'] }}{% else %}now{% endif %}`, "now"},
		{`{% if fields['subject'] %}{% if fields['missing'] %}bad{% else %}nested{% endif %}{% endif %}`, "nested"},
		{`{{ fields['missing'] }} {{ fields['title_default'][80:90] }}`, " "},
	} {
		template, err := CompileFieldTemplate(fixture.raw)
		if err != nil {
			t.Fatal(fixture.raw, err)
		}
		value, err := template.Render(context.Background(), fields)
		if err != nil || value != fixture.want {
			t.Fatalf("%s => %q error=%v want=%q", fixture.raw, value, err, fixture.want)
		}
	}
}

func TestFieldTemplateLimits(t *testing.T) {
	for _, raw := range []string{`{{ fields['x']`, `{% if fields['x'] %}missing`, `{% else %}stray`, `{% include 'secret' %}`, `{{ system('command') }}`, `{{ fields.__class__ }}`, `{{ fields['x']|unknown }}`, strings.Repeat("{% if fields['x'] %}", 18) + strings.Repeat("{% endif %}", 18), strings.Repeat("x", (16<<10)+1)} {
		if _, err := CompileFieldTemplate(raw); err == nil {
			t.Fatal(raw)
		}
	}
	template, err := CompileFieldTemplate(`{{ fields['x'] }}{{ fields['x'] }}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := template.Render(context.Background(), map[string]string{"x": strings.Repeat("x", 40000)}); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := template.Render(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBundledFieldTemplatesCompile(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, definition := range catalog.Indexers {
		var rules struct {
			Fields map[string]struct {
				Text json.RawMessage `json:"text"`
			} `json:"fields"`
		}
		if len(definition.Torrents) == 0 {
			continue
		}
		if err := json.Unmarshal(definition.Torrents, &rules); err != nil {
			t.Fatal(err)
		}
		for field, rule := range rules.Fields {
			var raw string
			if len(rule.Text) == 0 || json.Unmarshal(rule.Text, &raw) != nil {
				continue
			}
			count++
			if _, err := CompileFieldTemplate(raw); err != nil {
				t.Errorf("%s %s: %v", definition.ID, field, err)
			}
		}
	}
	if count < 200 {
		t.Fatal(count)
	}
	t.Logf("compiled %d string field templates", count)
}
