package mediaserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestPlaybackLinksAllServersKeepCredentialsOutAndPreservePrefix(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin", "plex"} {
		t.Run(kind, func(t *testing.T) {
			client, err := New(kind, "https://media.test/prefix", "private-key", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || strings.Contains(r.URL.String(), "private-key") {
					t.Fatal(r.URL)
				}
				if kind == "plex" {
					if r.Header.Get("X-Plex-Token") != "private-key" {
						t.Fatal(r.Header)
					}
					return reply(200, `<MediaContainer machineIdentifier="machine"/>`), nil
				}
				if r.Header.Get("X-Emby-Token") != "private-key" {
					t.Fatal(r.Header)
				}
				return reply(200, `{"Id":"server"}`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			link, err := client.PlaybackURL(context.Background(), "item-id", "")
			if err != nil || !strings.HasPrefix(link, "https://media.test/prefix/web/") || strings.Contains(link, "private-key") {
				t.Fatal(link, err)
			}
			for _, host := range []string{"https://user:password@play.test", "https://play.test?token=bad", "javascript:alert(1)"} {
				if link, err := client.PlaybackURL(context.Background(), "item-id", host); err == nil {
					t.Fatal("unsafe playback host", link)
				}
			}
			if _, err := client.PlaybackURL(context.Background(), "../unsafe", ""); err == nil {
				t.Fatal("unsafe item id")
			}
		})
	}
}
