package notificationtemplate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSavedWebhookTemplateBodies(t *testing.T) {
	variables := map[string]any{"user_id": int64(9007199254740993), "title": "电影🌙", "text": "第一行\n\"<&>'", "medias": []map[string]any{{"title": "A", "vote": "8"}, {"title": "B", "vote": "9"}}}
	for _, tc := range []struct{ source, want string }{
		{`{{ user_id|tojson }}`, "9007199254740993"},
		{`{{ title|tojson }}`, `"\u7535\u5f71\ud83c\udf19"`},
		{`{{ text|tojson }}`, `"\u7b2c\u4e00\u884c\n\"\u003c\u0026\u003e\u0027"`},
		{`{% for m in medias %}{{ m.title }}:{{ m.vote }}{% if not loop.last %},{% endif %}{% endfor %}`, "A:8,B:9"},
		{`{{ medias|tojson }}`, `[{"title": "A", "vote": "8"}, {"title": "B", "vote": "9"}]`},
		{`{% for i in range(2, -1, -1) %}{{ i }}{% endfor %}`, "210"},
		{`{{ title|safe }}`, "电影🌙"},
		{`{% macro item(x) %}{{ x.title|upper }}{% endmacro %}{{ item(medias[1]) }}`, "B"},
		{"{{ medias[0]|tojson(indent=2) }}\n", "{\n  \"title\": \"A\",\n  \"vote\": \"8\"\n}"},
	} {
		data, err := json.Marshal(input{tc.source, variables})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := render(bytes.NewReader(data), &out); err != nil || out.String() != tc.want {
			t.Errorf("source=%q got=%q want=%q err=%v", tc.source, out.String(), tc.want, err)
		}
	}
}
func TestInvalidTemplateProducesNoSuccessfulBody(t *testing.T) {
	for _, source := range []string{`{% include 'external' %}`, `{{ title|unknown_filter }}`, `{% if title %}`, ""} {
		data, _ := json.Marshal(input{source, map[string]any{"title": "a"}})
		if err := render(bytes.NewReader(data), &strings.Builder{}); err == nil {
			t.Fatal(source)
		}
	}
}
