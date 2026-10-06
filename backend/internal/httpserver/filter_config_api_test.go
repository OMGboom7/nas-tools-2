package httpserver

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeFilterConfigurationWithoutLegacyBackend(t *testing.T) {
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("unexpected legacy call: %s", request.URL.Path)
		return nil, fmt.Errorf("legacy disabled")
	}))
	for _, step := range []struct {
		path     string
		form     url.Values
		status   int
		contains string
	}{
		{"group/add", url.Values{"name": {"New group"}, "default": {"Y"}}, 200, `"id":3`},
		{"group/default", url.Values{"id": {"2"}}, 200, `"success":true`},
		{"group/default", url.Values{"id": {"invalid"}}, 400, ""},
		{"rule/update", url.Values{"group_id": {"3"}, "rule_name": {"HD rule"}, "rule_pri": {"1"}, "rule_include": {"1080p\nWEB"}, "rule_free": {"1.0 0.0"}}, 200, `"id":1`},
		{"rule/info", url.Values{"groupid": {"3"}, "ruleid": {"1"}}, 200, `"include":"1080p\nWEB"`},
		{"rule/share", url.Values{"id": {"3"}}, 200, `"string":"`},
		{"rule/import", url.Values{"content": {base64.StdEncoding.EncodeToString([]byte(`{"name":"Imported","rules":[{"name":"Legacy","pri":1,"include":null}]}`))}}, 200, `"id":4`},
		{"rule/import", url.Values{"content": {"not-base64"}}, 400, ""},
		{"rule/info", url.Values{"groupid": {"4"}, "ruleid": {"2"}}, 200, `"name":"Legacy"`},
		{"rule/update", url.Values{"group_id": {"3"}, "rule_id": {"1"}, "rule_name": {"Updated rule"}, "rule_pri": {"2"}}, 200, `"id":1`},
		{"rule/info", url.Values{"groupid": {"3"}, "ruleid": {"1"}}, 200, "Updated rule"},
		{"rule/update", url.Values{"group_id": {"2"}, "rule_id": {"1"}, "rule_name": {"Wrong group"}, "rule_pri": {"2"}}, 404, ""},
		{"rule/delete", url.Values{"id": {"1"}}, 200, `"success":true`},
		{"rule/info", url.Values{"groupid": {"3"}, "ruleid": {"1"}}, 200, `"info":{}`},
		{"group/delete", url.Values{"id": {"3"}}, 200, `"success":true`},
		{"list", nil, 200, `"initRules":[`},
		{"group/restore", url.Values{"groupids": {`["1000",1001,"9999"]`}, "init_rulegroups": {`[{"sql":["DROP TABLE CONFIG_FILTER_GROUP"]}]`}}, 200, `"success":true`},
		{"list", nil, 200, `"name":"日常观影"`},
		{"rule/info", url.Values{"groupid": {"1000"}, "ruleid": {"10000"}}, 200, `"name":"1080p特效-bluray"`},
		{"group/restore", url.Values{"groupids": {"1000", "1001"}}, 200, `"success":true`},
		{"group/restore", url.Values{"groupids": {"12345"}}, 400, ""},
	} {
		response := performFormRequest(handler, "/api/v1/filterrule/"+step.path, token, step.form)
		if response.Code != step.status || !strings.Contains(response.Body.String(), step.contains) {
			t.Fatalf("%s: %d %s", step.path, response.Code, response.Body.String())
		}
	}
	form := url.Values{"name": {"No access"}, "default": {"N"}}
	if response := performFormRequest(handler, "/api/v1/filterrule/group/add", "", form); response.Code != 401 {
		t.Fatalf("unauthenticated mutation: %d", response.Code)
	}
	response := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if response.Code != 200 {
		t.Fatalf("create viewer: %d %s", response.Code, response.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	if response := performFormRequest(handler, "/api/v1/filterrule/group/add", viewer, form); response.Code != 403 {
		t.Fatalf("viewer mutation: %d", response.Code)
	}
}
