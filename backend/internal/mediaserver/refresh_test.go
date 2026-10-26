package mediaserver

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func refreshTestItem() RefreshItem {
	return RefreshItem{Title: "Movie", Year: "2026", Type: "MOV", Category: "Drama", TMDBID: "100", TargetRoot: "/media", TargetPath: "/media/Drama/Movie/movie.mkv"}
}
func TestRefreshRootAcrossProvidersUsesHeaderCredentialsAndNeverForcesMetadata(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin", "plex"} {
		t.Run(kind, func(t *testing.T) {
			writes := 0
			client, err := New(kind, "https://refresh.test/prefix", "private-refresh-token", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "refresh.test" || strings.Contains(r.URL.String(), "private") || r.Header.Get("X-Emby-Token")+r.Header.Get("X-Plex-Token") != "private-refresh-token" {
					t.Fatal("credential scope changed", r.URL)
				}
				if r.Method == "POST" {
					writes++
					if r.URL.Query().Get("force") != "" || strings.Contains(r.URL.Path, "emptyTrash") || r.URL.Query().Get("ReplaceAllMetadata") == "true" {
						t.Fatal("scan expanded", r.URL)
					}
					if kind == "plex" && r.URL.Path != "/prefix/library/sections/1/refresh" {
						t.Fatal(r.URL)
					}
					if kind == "emby" && r.URL.Path != "/prefix/emby/Library/Refresh" {
						t.Fatal(r.URL)
					}
					if kind == "jellyfin" && r.URL.Path != "/prefix/Library/Refresh" {
						t.Fatal(r.URL)
					}
					return reply(204, ""), nil
				}
				if r.Method != "GET" {
					t.Fatal(r.Method)
				}
				if strings.HasSuffix(r.URL.Path, "/library/sections") {
					return reply(200, `{"MediaContainer":{"size":1,"Directory":[{"key":"1","title":"Movies","Location":[{"path":"/media"}]}]}}`), nil
				}
				if kind == "plex" {
					return reply(200, `{"MediaContainer":{"machineIdentifier":"server-1"}}`), nil
				}
				return reply(200, `{"Id":"server-1"}`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			p, err := client.PlanRefresh(t.Context(), nil, true)
			if err != nil || p.ServerID != "server-1" || len(p.Targets) != 1 || writes != 0 {
				t.Fatal(p, err, writes)
			}
			if !ValidRefreshPlan(p) {
				t.Fatal("invalid native plan", p)
			}
			if err := client.SendRefresh(t.Context(), p.Targets[0]); err != nil || writes != 1 {
				t.Fatal(err, writes)
			}
		})
	}
}
func TestRefreshEmbyPreservesExistingMovieSeriesCategoryAndRootChoices(t *testing.T) {
	for _, tc := range []struct{ name, kind, items, folders, wantKind, wantID string }{
		{"existing-movie", "MOV", `[{"Id":"movie","Name":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"100"}}]`, "", "", ""},
		{"existing-series", "TV", `[{"Id":"show","Name":"Movie","ProductionYear":2026}]`, "", "item", "show"},
		{"classified-child", "MOV", "[]", `[{"Id":"films","Name":"Movies","SubFolders":[{"Id":"drama","Path":"/media/Drama"}]}]`, "item", "drama"},
		{"classified-parent", "MOV", "[]", `[{"Id":"films","Name":"Movies","SubFolders":[{"Id":"a","Path":"/media/a/Drama"},{"Id":"b","Path":"/media/b/Drama"}]}]`, "item", "films"},
		{"direct-beats-unrelated-fallback", "MOV", "[]", `[{"Id":"other","SubFolders":[{"Id":"x","Path":"/other/Drama"}]},{"Id":"films","Name":"Movies","SubFolders":[{"Id":"drama","Path":"/media/Drama"}]}]`, "item", "drama"},
		{"unmapped-root", "MOV", "[]", "[]", "root", "root"},
		{"foreign-tmdb-not-adopted", "MOV", `[{"Id":"foreign","Name":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"999"}}]`, "[]", "root", "root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := refreshTestItem()
			item.Type = tc.kind
			var rows []any
			if json.Unmarshal([]byte(tc.items), &rows) != nil {
				t.Fatal("fixture")
			}
			client, _ := New("emby", "https://refresh.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("planning wrote", r.Method)
				}
				switch r.URL.Path {
				case "/emby/System/Info":
					return reply(200, `{"Id":"server-1"}`), nil
				case "/emby/Items":
					if r.URL.Query().Get("SearchTerm") != "Movie" || r.URL.Query().Get("StartIndex") != "0" {
						t.Fatal(r.URL)
					}
					b, _ := json.Marshal(map[string]any{"Items": rows, "TotalRecordCount": len(rows)})
					return reply(200, string(b)), nil
				case "/emby/Library/SelectableMediaFolders":
					return reply(200, tc.folders), nil
				default:
					t.Fatal(r.URL)
					return nil, ErrResponse
				}
			}))
			p, err := client.PlanRefresh(t.Context(), []RefreshItem{item}, false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantID == "" {
				if len(p.Targets) != 0 {
					t.Fatal(p)
				}
				return
			}
			if len(p.Targets) != 1 || p.Targets[0].Kind != tc.wantKind || p.Targets[0].ID != tc.wantID {
				t.Fatal(p)
			}
		})
	}
}
func TestRefreshPlexUsesCompleteJSONOrXMLLocationsAndPathBoundaries(t *testing.T) {
	for _, xmlMode := range []bool{false, true} {
		for _, tc := range []struct {
			name, path, expected string
			directory, whole     bool
			count                int
		}{
			{"file-directory", "/media/Movie/a.mkv", "/media/Movie", false, false, 1},
			{"complete-disc", "/media/Movie.v1", "/media/Movie.v1", true, false, 1},
			{"longest-root", "/media/nested/Movie/a.mp4", "/media/nested/Movie", false, false, 1},
			{"sibling-is-not-child", "/media-other/Movie/a.mp4", "", false, false, 2},
			{"whole", "", "", false, true, 2},
		} {
			t.Run(tc.name+map[bool]string{false: "-json", true: "-xml"}[xmlMode], func(t *testing.T) {
				client, _ := New("plex", "https://refresh.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != "GET" {
						t.Fatal("planning wrote")
					}
					if r.URL.Path == "/" {
						return reply(200, `{"MediaContainer":{"machineIdentifier":"server-1"}}`), nil
					}
					if xmlMode {
						return reply(200, `<MediaContainer size="2"><Directory key="1" title="Movies"><Location path="/media"/></Directory><Directory key="2" title="Nested"><Location path="/media/nested"/></Directory></MediaContainer>`), nil
					}
					return reply(200, `{"MediaContainer":{"size":2,"Directory":[{"key":"1","title":"Movies","Location":[{"path":"/media"}]},{"key":"2","title":"Nested","Location":[{"path":"/media/nested"}]}]}}`), nil
				}))
				items := []RefreshItem{}
				if !tc.whole {
					v := refreshTestItem()
					v.TargetPath = tc.path
					v.TargetRoot = "/"
					v.Directory = tc.directory
					items = append(items, v)
				}
				p, err := client.PlanRefresh(t.Context(), items, tc.whole)
				if err != nil || len(p.Targets) != tc.count {
					t.Fatal(p, err)
				}
				if tc.expected != "" && p.Targets[0].Path != tc.expected {
					t.Fatal(p)
				}
				if tc.name == "longest-root" && p.Targets[0].ID != "2" {
					t.Fatal(p)
				}
			})
		}
	}
}
func TestRefreshPlanningRejectsIncompleteOrUnsafeResultsWithoutEscalating(t *testing.T) {
	for _, body := range []string{
		`{"MediaContainer":{"size":1,"totalSize":2,"Directory":[{"key":"1"}]}}`,
		`{"MediaContainer":{"size":1,"Directory":[{"key":"../escape"}]}}`,
		`{"MediaContainer":{"size":1,"Directory":[{"key":"1","Location":[{"path":"relative"}]}]}}`,
		`{"MediaContainer":{"size":2,"Directory":[{"key":"1"},{"key":"1"}]}}`,
		`{"MediaContainer":null}`,
		strings.Repeat("x", (1<<20)+1),
	} {
		client, _ := New("plex", "https://refresh.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Fatal("invalid readonly response caused a write")
			}
			if r.URL.Path == "/" {
				return reply(200, `{"MediaContainer":{"machineIdentifier":"server-1"}}`), nil
			}
			return reply(200, body), nil
		}))
		if _, err := client.PlanRefresh(t.Context(), nil, true); err == nil {
			t.Fatal("accepted incomplete plan")
		}
	}
}
func TestRefreshFailuresCancellationAndRedirectsDoNotRetryOrSwitchVerbs(t *testing.T) {
	for _, code := range []int{302, 401, 403, 500} {
		calls := 0
		client, _ := New("emby", "https://refresh.test", "private-token", transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "refresh.test" || r.Method != "POST" {
				t.Fatal("retried elsewhere", r.URL)
			}
			resp := reply(code, "private upstream error")
			resp.Header.Set("Location", "https://evil.test")
			return resp, nil
		}))
		err := client.SendRefresh(t.Context(), RefreshTarget{Kind: "root", ID: "root"})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatal(err, calls)
		}
	}
	calls := 0
	client, _ := New("emby", "https://refresh.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.SendRefresh(ctx, RefreshTarget{Kind: "root", ID: "root"}); err == nil || calls > 1 {
		t.Fatal(err, calls)
	}
}
func TestRefreshNormalizedPathsAndStableDeduplication(t *testing.T) {
	for _, bad := range []string{"relative", "/media/../outside", "/media/\x00", "https://evil.test/path"} {
		if _, ok := RefreshPath(bad); ok {
			t.Fatal(bad)
		}
	}
	if !refreshContains(`C:\Media`, "c:/media/Movie/a.mkv") || refreshContains("/media", "/media-two/Movie") {
		t.Fatal("path boundary")
	}
	tg := RefreshTarget{Kind: "item", ID: "id", Label: "label"}
	if !reflect.DeepEqual(uniqueRefreshTargets([]RefreshTarget{tg, tg}), []RefreshTarget{tg}) {
		t.Fatal("duplicate target")
	}
	if ValidRefreshPlan(RefreshPlan{Server: "plex", ServerID: "server", Targets: []RefreshTarget{{Kind: "section", ID: "0"}}}) {
		t.Fatal("zero section")
	}
}
func TestRefreshEmbyRepeatedEpisodesShareOneIdentityQueryAndOneTarget(t *testing.T) {
	queries := 0
	client, _ := New("emby", "https://refresh.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/System/Info") {
			return reply(200, `{"Id":"server-1"}`), nil
		}
		queries++
		return reply(200, `{"Items":[{"Id":"show","Name":"Movie","ProductionYear":2026}],"TotalRecordCount":1}`), nil
	}))
	item := refreshTestItem()
	item.Type = "TV"
	items := make([]RefreshItem, RefreshItemLimit)
	for i := range items {
		items[i] = item
	}
	p, err := client.PlanRefresh(t.Context(), items, false)
	if err != nil || queries != 1 || len(p.Targets) != 1 || p.Targets[0].ID != "show" {
		t.Fatal(p, err, queries)
	}
}
