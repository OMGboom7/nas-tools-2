package mediaserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestNativeInventoryChecksIdentityAndDoubleEpisodes(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			client, err := New(kind, "http://media.test/proxy", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Emby-Token") != "secret" || strings.Contains(request.URL.String(), "secret") {
					t.Fatalf("unsafe credentials: %s", request.URL.Path)
				}
				switch {
				case strings.HasSuffix(request.URL.Path, "/Users"):
					return reply(200, `[{"Id":"user1","Name":"viewer"}]`), nil
				case strings.HasSuffix(request.URL.Path, "/Items"):
					return reply(200, `{"Items":[{"Id":"wrong","Name":"Show","ProductionYear":2025,"ProviderIds":{"Tmdb":"999"}},{"Id":"series","Name":"Show","ProductionYear":2025,"ProviderIds":{"Tmdb":"100"}}],"TotalRecordCount":2}`), nil
				case strings.HasSuffix(request.URL.Path, "/Shows/series/Episodes"):
					if request.URL.Query().Get("IsMissing") != "false" {
						t.Fatal("missing episodes not excluded")
					}
					return reply(200, `{"Items":[{"ParentIndexNumber":0,"IndexNumber":0},{"ParentIndexNumber":1,"IndexNumber":1,"IndexNumberEnd":2},{"ParentIndexNumber":1,"IndexNumber":3,"IsVirtualItem":true}]}`), nil
				default:
					t.Fatalf("unexpected endpoint: %s", request.URL.Path)
				}
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Inventory(t.Context(), "viewer", Identity{Title: "Show", Year: "2025", TMDBID: "100", TV: true})
			if err != nil || len(result.ItemIDs) != 1 || result.ItemIDs[0] != "series" || !result.Episodes[0][0] || !result.Episodes[1][1] || !result.Episodes[1][2] || result.Episodes[1][3] {
				t.Fatalf("inventory=%+v err=%v", result, err)
			}
		})
	}
}

func TestInventoryDoesNotTurnFailuresIntoMissingMedia(t *testing.T) {
	for _, body := range []string{`{}`, `{"Items":[],"TotalRecordCount":101}`, `{"Items":[{"Id":"../unsafe","Name":"Movie"}]}`} {
		client, err := New("emby", "http://media.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) { return reply(200, body), nil }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Inventory(t.Context(), "", Identity{Title: "Movie"}); err == nil {
			t.Fatalf("invalid response accepted: %s", body)
		}
	}
	client, _ := New("emby", "http://media.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) {
		return reply(200, `{"Items":[],"TotalRecordCount":0}`), nil
	}))
	result, err := client.Inventory(t.Context(), "", Identity{Title: "Movie"})
	if err != nil || len(result.ItemIDs) != 0 {
		t.Fatalf("empty inventory=%+v err=%v", result, err)
	}
}
