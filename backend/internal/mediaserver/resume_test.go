package mediaserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestEmbyAndJellyfinResumeKeepTitlesProgressAndProtectedImages(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			client, err := New(kind, "https://media.example", "media-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Emby-Token") != "media-secret" || strings.Contains(request.URL.String(), "media-secret") {
					t.Fatalf("credential exposed: %s", request.URL.String())
				}
				switch request.URL.Path {
				case "/Users":
					return reply(200, `[{"Id":"user-1","Name":"alice","Policy":{"IsAdministrator":false}}]`), nil
				case "/Users/user-1/Items/Resume":
					if request.URL.Query().Get("Limit") != "12" || request.URL.Query().Get("MediaTypes") != "Video" {
						t.Fatalf("unexpected resume query: %s", request.URL.String())
					}
					return reply(200, `{"Items":[{"Id":"movie-1","Name":"电影","Type":"Movie","BackdropImageTags":["tag-1"],"UserData":{"PlayedPercentage":25}},{"Id":"episode-1","Type":"Episode","SeriesName":"剧集","SeriesId":"series-1","SeriesPrimaryImageTag":"series-tag","ParentIndexNumber":2,"IndexNumber":3,"UserData":{"PlayedPercentage":75}},{"Id":"other","Type":"Series"}]}`), nil
				case "/emby/System/Info", "/System/Info":
					return reply(200, `{"Id":"server-1"}`), nil
				default:
					t.Fatalf("unexpected resume path: %s", request.URL.String())
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			items, err := client.Resume(context.Background(), "alice", "", 12)
			if err != nil || len(items) != 2 || items[0].Name != "电影" || items[0].Percent != 25 || items[1].Name != "剧集 第2季第3集" || items[1].Percent != 75 || strings.Contains(items[0].Image, "media-secret") {
				t.Fatalf("Resume = %#v err=%v", items, err)
			}
			if kind == "emby" && !strings.Contains(items[1].Image, "/Items/series-1/Images/Backdrop") {
				t.Fatalf("Emby episode backdrop = %s", items[1].Image)
			}
			if kind == "jellyfin" && !strings.Contains(items[1].Image, "/Items/episode-1/Images/Primary") {
				t.Fatalf("Jellyfin episode image = %s", items[1].Image)
			}
		})
	}
}

func TestPlexResumeAcceptsXMLAndJSON(t *testing.T) {
	for _, format := range []string{"xml", "json"} {
		t.Run(format, func(t *testing.T) {
			client, err := New("plex", "https://plex.example", "plex-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Plex-Token") != "plex-secret" || strings.Contains(request.URL.String(), "plex-secret") {
					t.Fatalf("Plex token exposed: %s", request.URL.String())
				}
				switch request.URL.Path {
				case "/hubs/continueWatching/items":
					if request.URL.Query().Get("X-Plex-Container-Size") != "12" {
						t.Fatalf("Plex resume is unbounded: %s", request.URL.String())
					}
					if format == "json" {
						return reply(200, `{"MediaContainer":{"Metadata":[{"key":"/library/metadata/1","type":"movie","title":"电影","art":"/library/metadata/1/art","viewOffset":250,"duration":1000},{"key":"/library/metadata/2","type":"episode","grandparentTitle":"剧集","parentIndex":2,"index":3,"parentArt":"/library/metadata/2/art","viewOffset":750,"duration":1000}]}}`), nil
					}
					return reply(200, `<MediaContainer><Metadata key="/library/metadata/1" type="movie" title="电影" art="/library/metadata/1/art" viewOffset="250" duration="1000"/><Metadata key="/library/metadata/2" type="episode" grandparentTitle="剧集" parentIndex="2" index="3" parentArt="/library/metadata/2/art" viewOffset="750" duration="1000"/></MediaContainer>`), nil
				case "/":
					if format == "json" {
						return reply(200, `{"MediaContainer":{"machineIdentifier":"machine-1"}}`), nil
					}
					return reply(200, `<MediaContainer machineIdentifier="machine-1"/>`), nil
				default:
					t.Fatalf("unexpected Plex path: %s", request.URL.String())
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			items, err := client.Resume(context.Background(), "", "", 12)
			if err != nil || len(items) != 2 || items[0].Percent != 25 || items[1].Percent != 75 || items[1].Name != "剧集 第2季第3集" || !strings.Contains(items[1].Image, "/library/metadata/2/art") {
				t.Fatalf("Plex Resume = %#v err=%v", items, err)
			}
		})
	}
}
