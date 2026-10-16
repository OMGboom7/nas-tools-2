package mediaserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotEmbyJellyfinPaginationNestedItemsAndEpisodes(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			pages := 0
			prefix := "/proxy"
			if kind == "emby" {
				prefix += "/emby"
			}
			client, err := New(kind, "https://server.test/proxy", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.Header.Get("X-Emby-Token") != "private" || strings.Contains(r.URL.String(), "private") {
					t.Fatal("unsafe request", r.URL)
				}
				switch r.URL.Path {
				case prefix + "/Users":
					return reply(200, `[{"Id":"user","Name":"chosen"},{"Id":"admin","Name":"other","Policy":{"IsAdministrator":true}}]`), nil
				case prefix + "/Users/user/Views":
					return reply(200, `{"Items":[{"Id":"films","Name":"电影","CollectionType":"movies"},{"Id":"tv","Name":"剧集","CollectionType":"tvshows"},{"Id":"audio","Name":"音乐","CollectionType":"music"}],"TotalRecordCount":3}`), nil
				case prefix + "/Users/user/Items":
					q := r.URL.Query()
					if q.Get("Recursive") != "true" || q.Get("Limit") != "100" || q.Get("EnableTotalRecordCount") != "true" {
						t.Fatal(q)
					}
					if q.Get("ParentId") == "tv" {
						return reply(200, `{"Items":[{"Id":"show","Name":"Series","Type":"Series","ProviderIds":{"Tmdb":"99"}}],"TotalRecordCount":1}`), nil
					}
					if q.Get("ParentId") != "films" || q.Get("IncludeItemTypes") != "Movie" {
						t.Fatal(q)
					}
					start, _ := strconv.Atoi(q.Get("StartIndex"))
					pages++
					n := 100
					if start == 100 {
						n = 1
					}
					items := []map[string]any{}
					for i := 0; i < n; i++ {
						items = append(items, map[string]any{"Id": fmt.Sprint("m", start+i), "Name": "Movie", "Type": "Movie", "ProductionYear": 2026, "ProviderIds": map[string]string{"Tmdb": "100"}, "Path": "/nested/Movie.mkv"})
					}
					raw, _ := json.Marshal(map[string]any{"Items": items, "TotalRecordCount": 101})
					return reply(200, string(raw)), nil
				case prefix + "/Shows/show/Episodes":
					if r.URL.Query().Get("UserId") != "user" || r.URL.Query().Get("IsMissing") != "false" {
						t.Fatal(r.URL)
					}
					return reply(200, `{"Items":[{"Id":"e1","Type":"Episode","ParentIndexNumber":1,"IndexNumber":1,"IndexNumberEnd":2},{"Id":"e2","Type":"Episode","ParentIndexNumber":1,"IndexNumber":2}],"TotalRecordCount":2}`), nil
				default:
					t.Fatal("unexpected", r.URL)
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			var progress SyncProgress
			items, err := client.CollectSnapshotWithProgress(context.Background(), "chosen", []string{"films", "tv"}, func(p SyncProgress) error {
				if p.PagesRead < progress.PagesRead || p.ItemsDone < progress.ItemsDone || p.LibrariesDone < progress.LibrariesDone {
					t.Fatal("regressed progress", p, progress)
				}
				progress = p
				return nil
			})
			if err != nil || len(items) != 102 || pages != 2 || items[0].TMDBID != "100" || items[0].Path != "/nested/Movie.mkv" || len(items[101].Episodes) != 2 {
				t.Fatal(len(items), pages, err)
			}
			if progress.Stage != "publishing" || progress.LibrariesDone != 2 || progress.ItemsDone != 102 || progress.EpisodesDone != 2 || progress.PagesRead != 4 {
				t.Fatal(progress)
			}
		})
	}
}

func TestSnapshotPlexJSONAndXML(t *testing.T) {
	for _, format := range []string{"json", "xml"} {
		t.Run(format, func(t *testing.T) {
			client, err := New("plex", "http://plex.test/proxy", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Plex-Token") != "private" || strings.Contains(r.URL.String(), "private") {
					t.Fatal(r.URL)
				}
				var jsonBody, xmlBody string
				switch r.URL.Path {
				case "/proxy/library/sections":
					jsonBody = `{"MediaContainer":{"size":1,"Directory":[{"key":"tv","title":"TV","type":"show"}]}}`
					xmlBody = `<MediaContainer size="1"><Directory key="tv" title="TV" type="show"/></MediaContainer>`
				case "/proxy/library/sections/tv/all":
					if r.URL.Query().Get("includeGuids") != "1" || r.URL.Query().Get("X-Plex-Container-Start") != "0" {
						t.Fatal(r.URL)
					}
					jsonBody = `{"MediaContainer":{"size":1,"totalSize":1,"Metadata":[{"ratingKey":"show","type":"show","title":"Series","year":2026,"Guid":[{"id":"tmdb://99"}]}]}}`
					xmlBody = `<MediaContainer size="1" totalSize="1"><Directory ratingKey="show" type="show" title="Series" year="2026"><Guid id="tmdb://99"/></Directory></MediaContainer>`
				case "/proxy/library/metadata/show/allLeaves":
					jsonBody = `{"MediaContainer":{"size":1,"totalSize":1,"Metadata":[{"ratingKey":"ep","type":"episode","parentIndex":0,"index":1,"indexEnd":2}]}}`
					xmlBody = `<MediaContainer size="1" totalSize="1"><Video ratingKey="ep" type="episode" parentIndex="0" index="1" indexEnd="2"/></MediaContainer>`
				default:
					t.Fatal(r.URL)
				}
				if format == "xml" {
					return reply(200, xmlBody), nil
				}
				return reply(200, jsonBody), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			items, err := client.CollectSnapshot(context.Background(), "", []string{"tv"})
			if err != nil || len(items) != 1 || items[0].TMDBID != "99" || len(items[0].Episodes) != 2 || items[0].Episodes[0].Season != 0 {
				t.Fatal(items, err)
			}
		})
	}
}

func TestSnapshotRejectsMissingChangingAndIgnoredPagination(t *testing.T) {
	for _, mode := range []string{"missing-total", "changing-total", "repeated-id", "empty-page", "bad-envelope", "virtual", "wrong-type"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := New("emby", "http://server.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/emby/Users":
					return reply(200, `[{"Id":"u","Name":"user"}]`), nil
				case "/emby/Users/u/Views":
					return reply(200, `{"Items":[{"Id":"films","Name":"Films","CollectionType":"movies"}],"TotalRecordCount":1}`), nil
				default:
					if mode == "missing-total" {
						return reply(200, `{"Items":[]}`), nil
					}
					if mode == "bad-envelope" {
						return reply(200, `{"error":"no"}`), nil
					}
					if mode == "virtual" {
						return reply(200, `{"Items":[{"Id":"m","Name":"Movie","Type":"Movie","IsVirtualItem":true}],"TotalRecordCount":1}`), nil
					}
					if mode == "wrong-type" {
						return reply(200, `{"Items":[{"Id":"m","Name":"Movie","Type":"Episode"}],"TotalRecordCount":1}`), nil
					}
					if r.URL.Query().Get("StartIndex") == "0" {
						return reply(200, `{"Items":[{"Id":"m","Name":"Movie","Type":"Movie"}],"TotalRecordCount":2}`), nil
					}
					if mode == "changing-total" {
						return reply(200, `{"Items":[{"Id":"n","Name":"Movie","Type":"Movie"}],"TotalRecordCount":3}`), nil
					}
					if mode == "repeated-id" {
						return reply(200, `{"Items":[{"Id":"m","Name":"Movie","Type":"Movie"}],"TotalRecordCount":2}`), nil
					}
					return reply(200, `{"Items":[],"TotalRecordCount":2}`), nil
				}
			}))
			if items, err := client.CollectSnapshot(context.Background(), "user", []string{"films"}); err == nil || items != nil {
				t.Fatal(items, err)
			}
		})
	}
}
func TestSnapshotRejectsBadSelectionUserAndIncompleteEpisodes(t *testing.T) {
	for _, mode := range []string{"empty", "unknown", "duplicate", "missing-user", "bad-episode", "episode-failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, _ := New("jellyfin", "http://server.test", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/Users":
					return reply(200, `[{"Id":"u","Name":"user","Policy":{"IsAdministrator":true}}]`), nil
				case "/Users/u/Views":
					return reply(200, `{"Items":[{"Id":"tv","Name":"TV","CollectionType":"tvshows"}],"TotalRecordCount":1}`), nil
				case "/Users/u/Items":
					return reply(200, `{"Items":[{"Id":"show","Name":"Series","Type":"Series"}],"TotalRecordCount":1}`), nil
				default:
					if mode == "cancel" {
						cancel()
					}
					if mode == "episode-failure" {
						return reply(503, `{}`), nil
					}
					return reply(200, `{"Items":[{"Id":"ep","Type":"Episode","ParentIndexNumber":1}],"TotalRecordCount":1}`), nil
				}
			}))
			selected := []string{"tv"}
			user := "user"
			if mode == "empty" {
				selected = nil
			}
			if mode == "unknown" {
				selected = []string{"missing"}
			}
			if mode == "duplicate" {
				selected = []string{"tv", "tv"}
			}
			if mode == "missing-user" {
				user = "absent"
			}
			if _, err := client.CollectSnapshot(ctx, user, selected); err == nil {
				t.Fatal("accepted", mode)
			}
		})
	}
}
func TestSnapshotProgressFailureStopsCollectionWithoutSuccess(t *testing.T) {
	sentinel := fmt.Errorf("progress receipt unavailable")
	client, err := New("emby", "http://server.test", "key", transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/emby/Users":
			return reply(200, `[{"Id":"u","Name":"user"}]`), nil
		case "/emby/Users/u/Views":
			return reply(200, `{"Items":[{"Id":"films","Name":"Films","CollectionType":"movies"}],"TotalRecordCount":1}`), nil
		default:
			return reply(200, `{"Items":[{"Id":"m","Type":"Movie","Name":"Movie"}],"TotalRecordCount":1}`), nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	final := false
	items, err := client.CollectSnapshotWithProgress(context.Background(), "user", []string{"films"}, func(p SyncProgress) error {
		if p.Stage == "publishing" {
			final = true
		}
		if p.PagesRead == 1 {
			return sentinel
		}
		return nil
	})
	if err != sentinel || items != nil || final {
		t.Fatal(items, err, final)
	}
}
