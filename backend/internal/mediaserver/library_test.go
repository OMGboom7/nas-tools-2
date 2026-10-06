package mediaserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestLatestUsesMatchingUserAndKeepsCredentialOutOfURLs(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			prefix := "/prefix"
			if kind == "emby" {
				prefix += "/emby"
			}
			client, err := New(kind, "https://media.example/prefix", "media-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Emby-Token") != "media-secret" || strings.Contains(request.URL.String(), "media-secret") {
					t.Fatalf("credential exposed: %s", request.URL.String())
				}
				switch request.URL.Path {
				case "/prefix/Users":
					return reply(200, `[{"Id":"admin-id","Name":"admin","Policy":{"IsAdministrator":true}},{"Id":"alice-id","Name":"alice","Policy":{"IsAdministrator":false}}]`), nil
				case "/prefix/Users/alice-id/Items/Latest":
					if request.URL.Query().Get("Limit") != "18" || request.URL.Query().Get("MediaTypes") != "Video" {
						t.Fatalf("unexpected latest query: %s", request.URL.String())
					}
					return reply(200, `[{"Id":"movie-1","Name":"测试电影","Type":"Movie"},{"Id":"series-1","Name":"测试剧集","Type":"Series"},{"Id":"episode-1","Name":"跳过","Type":"Episode"}]`), nil
				case prefix + "/System/Info":
					return reply(200, `{"Id":"server-id"}`), nil
				default:
					t.Fatalf("unexpected latest path: %s", request.URL.String())
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			items, err := client.Latest(context.Background(), "alice", "https://play.example", 18)
			if err != nil || len(items) != 2 || items[0].Name != "测试电影" || items[1].Type != "电视剧" || !strings.Contains(items[0].Image, "/Items/movie-1/Images/Primary") || strings.Contains(items[0].Image, "media-secret") || !strings.Contains(items[0].Link, "serverId=server-id") {
				t.Fatalf("Latest = %#v err=%v", items, err)
			}
		})
	}
}

func TestPlexLatestAcceptsXMLAndJSONWithoutTokenInArtworkURL(t *testing.T) {
	for _, format := range []string{"xml", "json"} {
		t.Run(format, func(t *testing.T) {
			client, err := New("plex", "https://plex.example", "plex-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Plex-Token") != "plex-secret" || strings.Contains(request.URL.String(), "plex-secret") {
					t.Fatalf("Plex token exposed: %s", request.URL.String())
				}
				switch request.URL.Path {
				case "/library/recentlyAdded":
					if request.URL.Query().Get("X-Plex-Container-Size") != "18" || request.URL.Query().Get("X-Plex-Container-Start") != "0" {
						t.Fatalf("Plex latest is unbounded: %s", request.URL.String())
					}
					if format == "json" {
						return reply(200, `{"MediaContainer":{"Metadata":[{"key":"/library/metadata/1","type":"movie","title":"电影","thumb":"/library/metadata/1/thumb"},{"key":"/library/metadata/2","type":"season","parentTitle":"剧集","index":2,"parentThumb":"/library/metadata/2/thumb"}]}}`), nil
					}
					return reply(200, `<MediaContainer><Metadata key="/library/metadata/1" type="movie" title="电影" thumb="/library/metadata/1/thumb"/><Metadata key="/library/metadata/2" type="season" parentTitle="剧集" index="2" parentThumb="/library/metadata/2/thumb"/></MediaContainer>`), nil
				case "/":
					if format == "json" {
						return reply(200, `{"MediaContainer":{"machineIdentifier":"machine-1"}}`), nil
					}
					return reply(200, `<MediaContainer machineIdentifier="machine-1"/>`), nil
				default:
					t.Fatalf("unexpected Plex latest path: %s", request.URL.String())
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			items, err := client.Latest(context.Background(), "", "https://play.example", 18)
			if err != nil || len(items) != 2 || items[0].Name != "电影" || items[1].Name != "剧集 第2季" || items[1].Type != "电视剧" || !strings.Contains(items[0].Image, "/library/metadata/1/thumb") || strings.Contains(items[0].Image, "plex-secret") || !strings.Contains(items[0].Link, "machine-1") {
				t.Fatalf("Plex latest = %#v err=%v", items, err)
			}
		})
	}
}
