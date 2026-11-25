package mediaserver

import (
	"bytes"
	"net/http"
	"testing"
)

func TestArchivePlexAndJellyfinPagesKeepOriginalMarkdown(t *testing.T) {
	for _, kind := range []string{"plex", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			items := 0
			client, err := New(kind, "http://archive.test", "fixture", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/library/sections":
					return reply(200, `<MediaContainer size="1"><Directory key="7" title="TV" type="show"/></MediaContainer>`), nil
				case "/library/sections/7/all":
					items++
					if r.URL.Query().Get("X-Plex-Container-Start") == "0" {
						return reply(200, `<MediaContainer size="1" totalSize="2"><Directory ratingKey="3" title="Z" year="2010" type="show"/></MediaContainer>`), nil
					}
					return reply(200, `<MediaContainer size="1" totalSize="2"><Directory ratingKey="4" title="A" year="2010" type="show"><Guid id="imdb://tt42"/></Directory></MediaContainer>`), nil
				case "/Users":
					return reply(200, `[{"Id":"user","Name":"chosen"}]`), nil
				case "/Users/user/Views":
					return reply(200, `{"Items":[{"Id":"7","Name":"TV","CollectionType":"tvshows"}],"TotalRecordCount":1}`), nil
				case "/Users/user/Items":
					items++
					if r.URL.Query().Get("StartIndex") == "0" {
						return reply(200, `{"Items":[{"Id":"3","Name":"Z","Type":"Series","ProductionYear":2010}],"TotalRecordCount":2}`), nil
					}
					return reply(200, `{"Items":[{"Id":"4","Name":"A","Type":"Series","ProductionYear":2010,"ProviderIds":{"Imdb":"tt42"}}],"TotalRecordCount":2}`), nil
				default:
					t.Fatal("unexpected archive request", r.URL.Path)
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			count, err := client.WriteArchive(t.Context(), "chosen", &buf)
			if err != nil || count != 1 || items != 2 || buf.String() != "- TV\n  - A (2010) [IMDB:tt42]\n  - Z (2010)\n" {
				t.Fatal(err, count, items, buf.String())
			}
		})
	}
}
