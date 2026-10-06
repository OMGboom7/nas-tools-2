package httpserver

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestCustomWordsSharingAPIWithoutFlask(t *testing.T) {
	handler, token, _ := nativeServicesFixture(t, "", nil, nil)
	form := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"2"}, "enabled": {"1"}, "new_replaced": {"Original"}, "new_replace": {"Corrected"}}
	response := performFormRequest(handler, "/api/v1/words/item/update", token, form)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = performFormRequest(handler, "/api/v1/words/item/export", token, url.Values{"ids_info": {"-1_1"}, "note": {"分享说明"}})
	var result struct {
		String string `json:"string"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.String == "" {
		t.Fatalf("export=%d %s", response.Code, response.Body.String())
	}
	response = performFormRequest(handler, "/api/v1/words/item/analyse", token, url.Values{"import_code": {result.String}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"note_string":"分享说明"`) {
		t.Fatalf("analyse=%d %s", response.Code, response.Body.String())
	}
	performFormRequest(handler, "/api/v1/words/item/delete", token, url.Values{"id": {"1"}})
	response = performFormRequest(handler, "/api/v1/words/item/import", token, url.Values{"import_code": {result.String}, "ids_info": {`["-1_1"]`}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("import=%d %s", response.Code, response.Body.String())
	}
	response = performFormRequest(handler, "/api/v1/words/item/import", token, url.Values{"import_code": {result.String}, "ids_info": {`["-1_999"]`}})
	if response.Code != 400 {
		t.Fatalf("invalid selection=%d %s", response.Code, response.Body.String())
	}
}
