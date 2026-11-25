package builtinindexer

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMTeamFeedPromotionsSizeAndOriginalFields(t *testing.T) {
	loc := time.FixedZone("local", 8*3600)
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, loc)
	body := `{"code":"0","data":{"data":[
	{"id":"9223372036854775806","name":"A & <B>","category":401,"createdDate":"2026-11-25 08:12:14","size":"1073741825","status":{"discount":"_2X_FREE","mallSingleFree":false}},
	{"id":2,"name":"Timed","category":"002","createdDate":"2026-11-24 22:00:00","size":1073741826,"status":{"discount":"NORMAL","mallSingleFree":{"endDate":"2026-11-25 10:00:00"}}},
	{"id":3,"size":1073741824,"status":{"discount":"FREE"}},
	{"id":4,"size":1073741825,"status":{"discount":"NORMAL","mallSingleFree":{"endDate":"2026-11-25 09:59:59"}}},
	{"id":5,"size":1073741825,"status":{"discount":"NOT_FREE","mallSingleFree":false}},
	{"id":6,"size":1073741825,"status":{"discount":"PERCENT_50","mallSingleFree":{"endDate":"invalid"}}},
	{"id":7,"size":1073741825,"status":null}
	]}}`
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://api.m-team.io/api/torrent/search" || r.Header.Get("X-Api-Key") != "fixture-key" || r.UserAgent() != "fixture-agent" || r.Header.Get("Cookie") != "" {
			t.Fatal("unexpected request", r.Method, r.URL)
		}
		var v map[string]any
		if json.NewDecoder(r.Body).Decode(&v) != nil || len(v) != 5 || v["mode"] != "movie" || v["pageNumber"] != float64(1) || v["pageSize"] != float64(100) || v["visible"] != float64(1) || v["keyword"] != "" {
			t.Fatal(v)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	rows, err := FetchMTeamFeedCategory(t.Context(), "https://kp.m-team.io", "fixture-key", "fixture-agent", "movie", 1<<30, now, transport)
	if err != nil || len(rows) != 2 {
		t.Fatal(len(rows), err)
	}
	data, err := EncodeMTeamFeed(rows, now)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Items []MTeamFeedItem `xml:"channel>item"`
	}
	if xml.Unmarshal(data, &got) != nil || len(got.Items) != 2 {
		t.Fatal(string(data))
	}
	item := got.Items[0]
	if item.Title != "A & <B>" || item.Enclosure.Length != 1073741825 || item.Link != "https://kp.m-team.io/detail/9223372036854775806" || item.Enclosure.URL != item.Link || item.GUID.Value != item.Link || item.GUID.Permalink || item.PubDate != "Wed, 25 Nov 2026 08:12:14 +0800" || item.Category != "401" || got.Items[1].Category != "002" {
		t.Fatal(item)
	}
	later, err := EncodeMTeamFeed(append(rows, rows...), now.Add(time.Second))
	if err != nil || strings.Contains(string(later), "/detail/2<") || strings.Count(string(later), "<item>") != 1 {
		t.Fatal(string(later), err)
	}

}
func TestMTeamFeedRejectsForeignOriginsAndInvalidResponses(t *testing.T) {
	for _, u := range []string{"https://m-team.cc.other.test", "https://other.test/m-team.cc", "https://user@kp.m-team.cc", "http://kp.m-team.cc", "https://kp.m-team.cc:9000"} {
		if _, ok := MTeamFeedOrigin(u); ok {
			t.Fatal(u)
		}
	}
	if v, ok := MTeamFeedOrigin("https://www.m-team.cc/login?from=site"); !ok || v != "https://www.m-team.cc" {
		t.Fatal(v, ok)
	}
	for _, body := range []string{`{"code":"1","data":{"data":[]}}`, `{"code":"0","data":{"data":null}}`, `{"code":"0","data":{"data":[{"id":"x","size":42,"name":"A","status":{"discount":"FREE"}}]}}`} {
		_, err := FetchMTeamFeedCategory(t.Context(), "https://kp.m-team.cc", "fixture", "", "normal", 0, time.Now(), fakeTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		if err == nil {
			t.Fatal(body)
		}
	}
}
