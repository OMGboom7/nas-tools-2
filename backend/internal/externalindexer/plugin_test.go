package externalindexer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPluginCommandDiscoveryAndPagedResults(t *testing.T) {
	for _, kind := range []string{"Jackett", "Prowlarr"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/prefix/api/v2.0/indexers":
					fmt.Fprint(w, `[{"id":"tracker","name":"Fixture","type":"private"}]`)
				case "/prefix/api/v1/indexerstats":
					fmt.Fprint(w, `{"indexers":[{"indexerName":"Fixture","indexerId":42}]}`)
				default:
					q := r.URL.Query()
					if q.Get("limit") != "100" || q.Get("offset") != "200" {
						t.Error("wrong page")
					}
					if kind == "Jackett" {
						if r.URL.Path != "/prefix/api/v2.0/indexers/tracker/results/torznab/" || q.Get("q") != "剧 & #" || q.Get("apikey") != "fixture" {
							t.Error("wrong Jackett request")
						}
						fmt.Fprint(w, strings.Replace(torznabFixture, "<item>", `<item><jackettindexer id="remote">Actual</jackettindexer><size>0123</size>`, 1))
					} else {
						if r.URL.Path != "/prefix/api/v1/search" || q.Get("query") != "剧 & #" || q.Get("indexerIds") != "42" || r.Header.Get("X-Api-Key") != "fixture" {
							t.Error("wrong Prowlarr request")
						}
						fmt.Fprint(w, `[{"indexerId":42,"indexer":"Actual","title":"Movie","downloadUrl":"https://fixture.invalid/grab?token=secret","guid":"https://fixture.invalid/details","size":123,"seeders":0}]`)
					}
				}
			}))
			defer server.Close()
			request := map[string]any{"Config": Config{Kind: kind, Host: server.URL + "/prefix", APIKey: "fixture"}}
			run := func(action string, target any) {
				t.Helper()
				input, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := PluginCommand(t.Context(), action, bytes.NewReader(input), &out); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(out.Bytes(), target); err != nil {
					t.Fatal(err)
				}
			}
			var indexers []Indexer
			run("discover", &indexers)
			if len(indexers) != 1 || indexers[0].Kind != kind {
				t.Fatal(indexers)
			}
			request["Domain"] = indexers[0].Domain
			request["Page"] = 2
			request["Keyword"] = "剧 & #"
			var rows []map[string]any
			run("search", &rows)
			if calls != 2 || len(rows) != 1 || len(rows[0]) != 13 {
				t.Fatal(rows, calls)
			}
			row := rows[0]
			if row["indexer"] != "Actual" || row["enclosure"] == "" {
				t.Fatal(row)
			}
			if kind == "Jackett" && (row["indexer_id"] != "remote" || row["size"] != "0123" || row["seeders"] != "7" || row["downloadvolumefactor"] != "0" || row["freeleech"] != true || row["imdbid"] != "tt12345") {
				t.Fatal(row)
			}
			if kind == "Prowlarr" && (row["indexer_id"] != float64(42) || row["size"] != float64(123) || row["seeders"] != float64(0) || row["freeleech"] != nil || row["imdbid"] != nil) {
				t.Fatal(row)
			}
			for _, bad := range []map[string]any{{"Page": -1}, {"Domain": "https://other.invalid/api/v1/indexer/42"}, {"url": "http://other.invalid"}} {
				for key, value := range bad {
					request[key] = value
				}
				input, _ := json.Marshal(request)
				var out bytes.Buffer
				if err := PluginCommand(t.Context(), "search", bytes.NewReader(input), &out); err == nil || out.Len() != 0 || calls != 2 {
					t.Fatal(bad)
				}
				request["Page"], request["Domain"] = 2, indexers[0].Domain
			}
		})
	}
}
