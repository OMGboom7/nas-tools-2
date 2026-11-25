package httpserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestTitleMatchSearchOrder(t *testing.T) {
	tests := []struct {
		name  string
		query titleMatchQuery
		want  string
	}{
		{"strict title", titleMatchQuery{Kind: "movie", FirstYear: "2020", Mode: "title"}, "movie:2020 movie:2021 movie:2019 tv:2020"},
		{"relaxed title", titleMatchQuery{Kind: "movie", FirstYear: "2020", Mode: "title", Relax: true}, "movie:2020 movie:2021 movie:2019 tv:2020 multi:"},
		{"unknown type", titleMatchQuery{Kind: "movie", Mode: "title"}, "multi:"},
		{"relaxed file", titleMatchQuery{Kind: "movie", FirstYear: "2020", Mode: "file", Relax: true}, "movie:2020 movie:2021 movie:2019 movie:"},
		{"file without year", titleMatchQuery{Kind: "movie", Mode: "file", Relax: true}, "movie:"},
		{"season and premiere", titleMatchQuery{Kind: "tv", Season: 2, SeasonYear: "2023", FirstYear: "2020", Mode: "title", Relax: true}, "tv: tv:2020 tv:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				q := r.URL.Query()
				if q.Get("query") != "剧名" || q.Get("language") != "zh" || q.Get("page") != "1" || q.Get("include_adult") != "true" {
					t.Fatal(q)
				}
				calls = append(calls, strings.TrimPrefix(r.URL.Path, "/3/search/")+":"+q.Get("year")+q.Get("first_air_date_year"))
				return jsonResponse(r, `{"results":[]}`), nil
			})}
			tc.query.Title = "剧名"
			got, err := matchMediaTitle(context.Background(), client, titleMatchSettings(), tc.query)
			if err != nil || got != nil || strings.Join(calls, " ") != tc.want {
				t.Fatal(calls, got, err)
			}
		})
	}
}

func titleMatchSettings() map[string]any {
	return map[string]any{"app": map[string]any{"rmt_tmdbkey": "fixture"}, "media": map[string]any{"tmdb_include_adult": true}}
}

func TestTitleMatchCandidates(t *testing.T) {
	const alternatives = `{"id":2,"name":"Series","seasons":[{"season_number":2,"air_date":"2023-01-01"}],"translations":{"translations":[{"data":{"name":"目标"}}]}}`
	const movieAlias = `{"id":1,"title":"Other","alternative_titles":{"titles":[{"title":"目标"}]}}`
	tests := []struct {
		name          string
		query         titleMatchQuery
		rows          string
		details       map[string]string
		wantID, paths string
		wantError     bool
	}{
		{"last mixed direct", titleMatchQuery{Title: "straße", Kind: "multi"}, `[{"media_type":"movie","id":4,"title":"Straße"},{"media_type":"person","id":5,"name":"STRASSE"},{"media_type":"tv","id":9007199254740993,"original_name":"Die STRASSE","extra":"kept"}]`, nil, "9007199254740993", "", false},
		{"last mixed alias within five", titleMatchQuery{Title: "目标", Kind: "multi"}, `[{"media_type":"movie","id":1},{"media_type":"person","id":8},{"media_type":"tv","id":2},{"media_type":"person","id":8},{"media_type":"person","id":8},{"media_type":"tv","id":9}]`, map[string]string{"movie/1": movieAlias, "tv/2": alternatives}, "2", "movie/1 tv/2", false},
		{"six eligible single aliases", titleMatchQuery{Title: "目标", Kind: "movie"}, `[{"id":1},{"id":2},{"id":3},{"id":4},{"id":5},{"id":6},{"id":7}]`, map[string]string{"movie/6": strings.Replace(movieAlias, `"id":1`, `"id":6`, 1)}, "6", "movie/1 movie/2 movie/3 movie/4 movie/5 movie/6", false},
		{"detail unavailable", titleMatchQuery{Title: "目标", Kind: "movie"}, `[{"id":1}]`, nil, "", "movie/1", true},
		{"season alias", titleMatchQuery{Title: "目标", Kind: "tv", Season: 2, SeasonYear: "2023", FirstYear: "2020"}, `[{"id":1,"name":"目标","first_air_date":"2020-02-02"},{"id":2,"first_air_date":"2019-05-05"}]`, map[string]string{"tv/1": strings.ReplaceAll(strings.Replace(alternatives, `"id":2`, `"id":1`, 1), "2023-", "2022-"), "tv/2": alternatives}, "2", "tv/1 tv/2", false},
		{"numeric substring", titleMatchQuery{Title: "1917", Kind: "movie"}, `[{"id":1,"title":"Film 1917"}]`, map[string]string{"movie/1": movieAlias}, "", "movie/1", false},
		{"significant punctuation", titleMatchQuery{Title: "A?B", Kind: "movie"}, `[{"id":1,"title":"A B"}]`, map[string]string{"movie/1": movieAlias}, "", "movie/1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var details []string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				path := strings.TrimPrefix(r.URL.Path, "/3/")
				if strings.HasPrefix(path, "search/") {
					return jsonResponse(r, `{"results":`+tc.rows+`}`), nil
				}
				details = append(details, path)
				if s := tc.details[path]; s != "" {
					return jsonResponse(r, s), nil
				}
				return nil, errors.New("fixture unavailable")
			})}
			got, err := matchMediaTitle(context.Background(), client, titleMatchSettings(), tc.query)
			if text(got["id"]) != tc.wantID || (err != nil) != tc.wantError || strings.Join(details, " ") != tc.paths {
				t.Fatal(got, err, details)
			}
			if tc.wantID == "9007199254740993" && got["extra"] != "kept" {
				t.Fatal(got)
			}
		})
	}
}

func TestTitleMatchIncompleteYear(t *testing.T) {
	for _, recover := range []bool{false, true} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Query().Get("year") {
			case "2020":
				return nil, errors.New("fixture unavailable")
			case "2021":
				if recover {
					return jsonResponse(r, `{"results":[{"id":2,"title":"OK","release_date":"2020-01-01"},{"id":3,"title":"O.K.","release_date":"2021-01-01"}]}`), nil
				}
			}
			return jsonResponse(r, `{"results":[]}`), nil
		})}
		got, err := matchMediaTitle(context.Background(), client, titleMatchSettings(), titleMatchQuery{Title: "OK", Kind: "movie", FirstYear: "2020"})
		if recover && (err != nil || text(got["id"]) != "3") || !recover && (err == nil || got != nil) {
			t.Fatal(got, err)
		}
	}
}

func TestSourceSubjectCommand(t *testing.T) {
	saved := http.DefaultTransport
	defer func() { http.DefaultTransport = saved }()
	var calls int
	var body, path string
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host+r.URL.Path != path {
			t.Fatal(r.URL.Host, r.URL.Path)
		}
		return jsonResponse(r, body), nil
	})
	run := func(input string, ok bool) string {
		t.Helper()
		var out bytes.Buffer
		err := SearchTitlesCommand(context.Background(), "", "subject", strings.NewReader(input), &out)
		if (err == nil) != ok || !ok && out.Len() != 0 {
			t.Fatal(input, err, out.String())
		}
		return out.String()
	}
	const db = `{"provider":"douban","kind":"tv","id":"42"}`
	const bg = `{"provider":"bangumi","kind":"tv","id":"42"}`
	path = "frodo.douban.com/api/v2/tv/42"
	body = `{"id":"42","title":"剧","type":"tv","extra":[9007199254740993],"cover_url":"https://qnmob13.example/a"}`
	result := run(db, true)
	if !strings.Contains(result, "9007199254740993") || !strings.Contains(result, "img1.example") {
		t.Fatal(result)
	}
	for _, s := range []string{`{"id":43,"title":"错"}`, `{"id":42,"title":"未知电视剧"}`, `{"id":42,"title":"剧","type":"movie"}`} {
		body = s
		run(db, false)
	}
	path = "api.bgm.tv/v0/subjects/42"
	body = `{"id":42,"name_cn":"中文名","staff":[9007199254740993]}`
	if result := run(bg, true); !strings.Contains(result, "9007199254740993") {
		t.Fatal(result)
	}
	body = `{"id":41,"name_cn":"另一个"}`
	run(bg, false)
	before := calls
	for _, input := range []string{strings.Replace(bg, "42", "../42", 1), strings.Replace(bg, "tv", "movie", 1), bg + bg} {
		run(input, false)
	}
	if calls != before {
		t.Fatal("invalid selector reached a provider")
	}
}
