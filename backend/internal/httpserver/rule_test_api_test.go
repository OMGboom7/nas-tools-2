package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"testing"
)

func TestRuleTestProcessesWordsAndPerEpisodeSizesWithoutPython(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO CONFIG_FILTER_GROUP (ID,GROUP_NAME,IS_DEFAULT) VALUES (1,'Test','Y')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO CONFIG_FILTER_RULES (ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE,EXCLUDE,SIZE_LIMIT) VALUES (1,1,'Test','7','Correct.*1080p','','1,3')`); err != nil {
		t.Fatal(err)
	}
	word := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"2"}, "enabled": {"1"}, "regex": {"0"}, "new_replaced": {"Wrong"}, "new_replace": {"Correct"}}
	if response := performFormRequest(handler, "/api/v1/words/item/update", token, word); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	for _, test := range []struct {
		title, subtitle, size string
		match                 bool
		order                 int
	}{
		{"Wrong.S01E01-E10.1080p", "", "20", true, 7},
		{"Wrong.Movie.2025.1080p", "", "20", false, 0},
		{"Wrong.1080p", "全十集", "20", true, 7},
		{"Wrong.S01.1080p", "", "40", true, 7},
		{"Wrong.S01E01-E10.1080p", "", "40", false, 0},
		{"Wrong.S01E01E02E03.1080p", "", "6", true, 7},
		{"Wrong.S01E01E02E03.1080p", "", "12", false, 0},
		{"[Group] Wrong [01-12v2] [1080p]", "", "24", true, 7},
		{"[Group] Wrong [01-12v2] [1080p]", "", "48", false, 0},
		{"Wrong Season 2 Episode 1-10 1080p", "", "20", true, 7},
	} {
		response := performFormRequest(handler, "/api/v1/service/rule/test", token, url.Values{"title": {test.title}, "subtitle": {test.subtitle}, "size": {test.size}})
		var result struct {
			Flag  bool
			Order int
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result.Flag != test.match || result.Order != test.order {
			t.Fatalf("%q: %d %s err=%v", test.title, response.Code, response.Body.String(), err)
		}
	}
	if _, err := database.Exec(`UPDATE CONFIG_FILTER_RULES SET INCLUDE='DoesNotMatch' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/service/rule/test", token, url.Values{"title": {"Wrong.1080p"}})
	if response.Code != 200 || !containsJSONBoolean(t, response.Body.Bytes(), "flag", false) {
		t.Fatal(response.Body.String())
	}
	for _, form := range []url.Values{{"title": {"Movie"}, "size": {"NaN"}}, {"title": {"Movie"}, "size": {"-1"}}, {"title": {"Show.E10-E01"}}, {"title": {"Movie"}, "rulegroup": {"invalid"}}} {
		if response := performFormRequest(handler, "/api/v1/service/rule/test", token, form); response.Code != 400 {
			t.Fatalf("invalid form=%v: %d %s", form, response.Code, response.Body.String())
		}
	}
	if response := performFormRequest(handler, "/api/v1/service/rule/test", "", url.Values{"title": {"Movie"}}); response.Code != 401 {
		t.Fatalf("unauthenticated=%d", response.Code)
	}
	response = performFormRequest(handler, "/api/v1/service/rule/test", token, url.Values{"title": {"Movie"}, "rulegroup": {"-1"}})
	if response.Code != 200 || !containsJSONBoolean(t, response.Body.Bytes(), "flag", true) {
		t.Fatalf("bypass=%d %s", response.Code, response.Body.String())
	}
	if response := performFormRequest(handler, "/api/v1/service/rule/test", token, url.Values{"title": {"Movie"}, "rulegroup": {"999"}}); response.Code != 404 {
		t.Fatalf("unknown group=%d %s", response.Code, response.Body.String())
	}
	if _, err := database.Exec(`UPDATE CONFIG_FILTER_RULES SET INCLUDE='[invalid' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/service/rule/test", token, url.Values{"title": {"Movie"}}); response.Code != 422 {
		t.Fatalf("invalid stored regex=%d %s", response.Code, response.Body.String())
	}
}

func containsJSONBoolean(t *testing.T, body []byte, field string, want bool) bool {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	got, ok := value[field].(bool)
	return ok && got == want
}
