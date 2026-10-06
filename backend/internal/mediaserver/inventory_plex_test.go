package mediaserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPlexInventoryAcceptsJSONAndXMLWithIdentityValidation(t *testing.T) {
	for _, xmlFormat := range []bool{false, true} {
		t.Run(fmt.Sprint(xmlFormat), func(t *testing.T) {
			client, err := New("plex", "http://plex.test/proxy", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Plex-Token") != "secret" || strings.Contains(request.URL.String(), "secret") {
					t.Fatal("Plex credentials not protected")
				}
				switch request.URL.Path {
				case "/proxy/library/sections":
					if xmlFormat {
						return reply(200, `<MediaContainer size="1"><Directory key="1" type="show"/></MediaContainer>`), nil
					}
					return reply(200, `{"MediaContainer":{"Directory":[{"key":"1","type":"show"}]}}`), nil
				case "/proxy/library/sections/1/all":
					if request.URL.Query().Get("title") != "Show" || request.URL.Query().Get("type") != "2" {
						t.Fatal("bad Plex selector")
					}
					if xmlFormat {
						return reply(200, `<MediaContainer size="2" totalSize="2"><Directory ratingKey="9" type="show" title="Show" year="2025"><Guid id="tmdb://999"/></Directory><Directory ratingKey="10" type="show" title="Show" year="2025"><Guid id="tmdb://100"/></Directory></MediaContainer>`), nil
					}
					return reply(200, `{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"9","type":"show","title":"Show","year":2025,"Guid":[{"id":"tmdb://999"}]},{"ratingKey":"10","type":"show","title":"Show","year":2025,"Guid":[{"id":"tmdb://100"}]}]}}`), nil
				case "/proxy/library/metadata/10/allLeaves":
					if xmlFormat {
						return reply(200, `<MediaContainer size="2" totalSize="2"><Video type="episode" parentIndex="0" index="0"/><Video type="episode" parentIndex="1" index="1" indexEnd="2"/></MediaContainer>`), nil
					}
					return reply(200, `{"MediaContainer":{"totalSize":2,"Metadata":[{"type":"episode","parentIndex":0,"index":0},{"type":"episode","parentIndex":1,"index":1,"indexEnd":2}]}}`), nil
				default:
					t.Fatalf("unexpected endpoint %s", request.URL.Path)
				}
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Inventory(t.Context(), "", Identity{Title: "Show", Year: "2025", TMDBID: "100", TV: true})
			if err != nil || len(result.ItemIDs) != 1 || result.ItemIDs[0] != "10" || !result.Episodes[0][0] || !result.Episodes[1][1] || !result.Episodes[1][2] {
				t.Fatalf("inventory=%+v err=%v", result, err)
			}
		})
	}
}

func TestPlexInventoryPaginationAndFailureHandling(t *testing.T) {
	calls := 0
	client, _ := New("plex", "http://plex.test", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Query().Get("X-Plex-Container-Start") == "0" {
			return reply(200, `{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"1","type":"movie","title":"Movie"}]}}`), nil
		}
		if request.URL.Query().Get("X-Plex-Container-Start") != "1" {
			t.Fatal("incorrect pagination offset")
		}
		return reply(200, `{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"2","type":"movie","title":"Movie"}]}}`), nil
	}))
	items, err := client.plexInventoryItems(t.Context(), "/library/sections/1/all", map[string][]string{})
	if err != nil || len(items) != 2 || calls != 2 {
		t.Fatalf("pagination=%+v calls=%d err=%v", items, calls, err)
	}
	for _, body := range []string{`{}`, `{"MediaContainer":{}}`, `<Error/>`, `{"MediaContainer":{"size":0,"totalSize":10}}`, `{"MediaContainer":{"size":1,"Metadata":[]}}`} {
		client, _ := New("plex", "http://plex.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) { return reply(200, body), nil }))
		if _, err := client.plexInventoryItems(t.Context(), "/library/sections/1/all", map[string][]string{}); err == nil {
			t.Fatalf("invalid response accepted %s", body)
		}
	}
}

func TestPlexInventoryRejectsBrokenLibraryEnvelope(t *testing.T) {
	for _, body := range []string{`{"MediaContainer":null}`, `{"MediaContainer":{"error":"failed"}}`, `<MediaContainer/>`} {
		client, _ := New("plex", "http://plex.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) { return reply(200, body), nil }))
		if _, err := client.Inventory(t.Context(), "", Identity{Title: "Movie"}); err == nil {
			t.Fatalf("failed library list accepted %s", body)
		}
	}
}
