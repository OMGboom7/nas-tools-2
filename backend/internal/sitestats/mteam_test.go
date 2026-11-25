package sitestats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Profile and torrent fields are intentionally larger than the exact integer
// range of a JSON float. Do not round them on the way to the original database.
const mteamProfileFixture = `{"code":"0","data":{"id":"17","username":"Fixture","role":10,"createdDate":"2020-01-02","memberStatus":{"lastBrowse":"2026-11-25 01:02:03"},"memberCount":{"uploaded":"9007199254740993","downloaded":"1.5 GiB","shareRate":"0","bonus":"13.75"}}}`

func TestMTeamStatisticsCommandAndSameHostCredentials(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	var calls []string
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Host != "api.m-team.cc" || r.Header.Get("Cookie") != "" || r.UserAgent() != "fixture-agent" {
			t.Fatal("unexpected request scope")
		}
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/favicon.ico" {
			if r.Method != "GET" || r.Header.Get("X-API-KEY") != "" {
				t.Fatal("credential on icon")
			}
			return response("<html>not an icon</html>"), nil
		}
		if r.Method != "POST" || r.Header.Get("X-API-KEY") != "fixture-key" {
			t.Fatal("missing API credential")
		}
		switch r.URL.Path {
		case "/api/member/profile":
			return response(mteamProfileFixture), nil
		case "/api/member/getUserTorrentList":
			var body struct {
				PageNumber, PageSize int
				UserID, Type         string
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.PageNumber != len(calls)-1 || body.PageSize != 200 || body.UserID != "17" || body.Type != "SEEDING" {
				t.Fatal(body)
			}
			return response(fmt.Sprintf(`{"data":{"total":2,"totalPages":"2","pageNumber":%d,"data":[{"torrent":{"id":%d,"size":"9007199254740993","status":{"seeders":"2"}}}]}}`, body.PageNumber, body.PageNumber)), nil
		default:
			t.Fatal("unexpected path", r.URL.Path)
			return nil, ErrOrigin
		}
	})
	input := `{"url":"https://api.m-team.cc/login","key":"fixture-key","ua":"fixture-agent"}`
	var out bytes.Buffer
	if err := MTeamStatisticsCommand(t.Context(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	type schedulerSnapshot Snapshot
	var s schedulerSnapshot
	if json.Unmarshal(out.Bytes(), &s) != nil || s.Username != "Fixture" || s.Level != "VIP" || s.Download != 1610612736 || s.Bonus != 13.75 || s.Upload != 9007199254740993 || s.Seeding != 2 || s.SeedingSize != 18014398509481986 || len(s.Seeds) != 2 || s.Favicon != "" || strings.Contains(out.String(), "fixture-key") || len(calls) != 4 {
		t.Fatal(s, calls)
	}
	for _, bad := range []string{`{"url":"https://example.invalid/m-team.cc","key":"secret"}`, `{"url":"http://kp.m-team.cc","key":"secret"}`, `{"url":"https://user@kp.m-team.cc","key":"secret"}`, `{"url":"https://kp.m-team.cc","key":""}`, input + " {}"} {
		out.Reset()
		if err := MTeamStatisticsCommand(t.Context(), strings.NewReader(bad), &out); err == nil || out.Len() != 0 || len(calls) != 4 {
			t.Fatal("invalid command produced output or sent a request")
		}
	}
}

func TestMTeamRejectsPartialOrConflictingPages(t *testing.T) {
	first := `{"data":{"pageNumber":1,"total":2,"totalPages":2,"data":[{"torrent":{"id":1,"size":7,"status":{"seeders":3}}}]}}`
	good := strings.ReplaceAll(strings.Replace(first, `"pageNumber":1`, `"pageNumber":2`, 1), `"id":1`, `"id":2`)
	for _, bad := range []string{first, strings.Replace(good, `"total":2`, `"total":3`, 1), strings.Replace(good, `"totalPages":2`, `"totalPages":1`, 1), strings.Replace(good, `"seeders":3`, `"seeders":-3`, 1), strings.Replace(good, `"data":`, `"code":1,"data":`, 1), good + " {}"} {
		call := 0
		client := Client{Origin: "https://kp.m-team.info", APIKey: "key", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			call++
			if call == 1 {
				return response(mteamProfileFixture), nil
			}
			if call == 2 {
				return response(first), nil
			}
			return response(bad), nil
		})}
		if s, err := MTeamStatistics(t.Context(), &client, "Fixture"); err == nil || s.UserID != "" || len(s.Seeds) != 0 || call != 3 {
			t.Fatal(s, err, call)
		}
	}
}

func TestMTeamTrafficFormats(t *testing.T) {
	for raw, want := range map[string]int64{"949,000 B": 949000, "1.5 B": 2, "2.5 B": 2, "0.25 KiB": 256, "9007199254740993": 9007199254740993} {
		if got, ok := mteamTrafficBytes(raw); !ok || got != want {
			t.Fatal(raw, got, ok)
		}
	}
	for _, raw := range []string{"-7", "invalid 1.5 GiB", "2.5 GiB?", "999999999999999999999", ""} {
		if _, ok := mteamTrafficBytes(raw); ok {
			t.Fatal(raw)
		}
	}
}
