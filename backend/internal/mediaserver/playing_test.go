package mediaserver

import (
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"net/http"
	"strings"
	"testing"
)

func TestObservePlayingReadsLiveServerAndPreservesDifferentBitrateUnits(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin", "plex"} {
		t.Run(kind, func(t *testing.T) {
			body := `[{"NowPlayingItem":{"MediaType":"Video","Bitrate":8000000,"MediaStreams":[{"BitRate":7000000},{"BitRate":1000000}]},"RemoteEndPoint":"8.8.8.8","PlayState":{"IsPaused":false}},{"NowPlayingItem":{"MediaType":"Video","Bitrate":1,"MediaStreams":[{"BitRate":1}]},"RemoteEndPoint":"192.168.1.2","PlayState":{"IsPaused":false}},{"NowPlayingItem":{"MediaType":"Video"},"RemoteEndPoint":"invalid","PlayState":{"IsPaused":true}},{"NowPlayingItem":{"MediaType":"Audio"}},{"Id":"inactive"}]`
			if kind == "plex" {
				body = `{"MediaContainer":{"size":4,"Metadata":[{"type":"movie","Player":{"state":"playing","address":"8.8.8.8"},"Media":[{"bitrate":8000}]},{"type":"episode","Player":{"state":"playing","address":"192.168.1.2"},"Media":[{"bitrate":1}]},{"type":"movie","Player":{"state":"paused","address":"invalid"}},{"type":"track"}]}}`
			}
			calls := 0
			client, err := New(kind, "https://sessions.test/base", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				path := "/base/Sessions"
				header := "X-Emby-Token"
				if kind == "emby" {
					path = "/base/emby/Sessions"
				}
				if kind == "plex" {
					path = "/base/status/sessions"
					header = "X-Plex-Token"
				}
				if r.Method != "GET" || r.URL.Path != path || r.Header.Get(header) != "private" || r.URL.RawQuery != "" {
					t.Fatal(r.URL, r.Method)
				}
				return reply(200, body), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ObservePlaying(t.Context(), speedlimit.Spec{IPv4: "192.168.0.0/16", IPv6: "fd00::/8"})
			if err != nil || got.Playing != 1 || got.Exempt != 1 || got.Bitrate != 8000000 || got.UnknownBitrate != 0 || calls != 1 {
				t.Fatal(got, err, calls)
			}
		})
	}
}
func TestObservePlayingFailureCannotBecomeIdle(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[{"NowPlayingItem":{"MediaType":"Video"}}]`, `[{"NowPlayingItem":{"MediaType":"Video"},"PlayState":{"IsPaused":false},"RemoteEndPoint":"unknown"}]`, `[{"NowPlayingItem":{"MediaType":"Video","Bitrate":-1},"PlayState":{"IsPaused":false},"RemoteEndPoint":"8.8.8.8"}]`, `[{"NowPlayingItem":{"MediaType":"Video"},"PlayState":{"IsPaused":true,"IsPaused":false},"RemoteEndPoint":"8.8.8.8"}]`} {
		t.Run(body, func(t *testing.T) {
			client, _ := New("emby", "media.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) { return reply(200, body), nil }))
			if _, err := client.ObservePlaying(t.Context(), speedlimit.Spec{}); err == nil {
				t.Fatal("failed snapshot accepted")
			}
		})
	}
	client, _ := New("emby", "media.test", "secret", transportFunc(func(*http.Request) (*http.Response, error) {
		return reply(200, `[{"NowPlayingItem":{"MediaType":"Video"},"PlayState":{"IsPaused":false},"RemoteEndPoint":"8.8.8.8"}]`), nil
	}))
	o, err := client.ObservePlaying(t.Context(), speedlimit.Spec{IPv4: "192.168.0.0/16", IPv6: "fd00::/8"})
	if err != nil || o.Playing != 1 || o.UnknownBitrate != 1 {
		t.Fatal("unknown video marked idle", o, err)
	}
}
func TestPlexXMLPlayingCompleteAndAmbiguousResponses(t *testing.T) {
	good := `<MediaContainer size="2"><Video><Player state="buffering" address="8.8.8.8"/><Media bitrate="8000"/></Video><Track/></MediaContainer>`
	rows, err := parsePlexSessions([]byte(good))
	if err != nil || len(rows) != 1 || rows[0].bitrate != 8000000 {
		t.Fatal(rows, err)
	}
	for _, raw := range []string{
		strings.Replace(good, `size="2"`, `size="3"`, 1), strings.Replace(good, `state="buffering"`, `state="playing" state="paused"`, 1),
		`<MediaContainer size="1"><Video><Media bitrate="8000"/></Video></MediaContainer>`,
		`<MediaContainer size="1"><Video><Player state="playing"/><Player state="paused"/></Video></MediaContainer>`,
		`<!DOCTYPE MediaContainer><MediaContainer size="0"/>`,
		`<MediaContainer size="0"><Unknown><Video/></Unknown></MediaContainer>`,
		`<MediaContainer size="0"/><MediaContainer size="1"><Video/></MediaContainer>`,
		`{"MediaContainer":{"size":1,"Metadata":[]}}`,
		`{"MediaContainer":{"size":0,"size":1,"Metadata":[]}}`,
	} {
		if _, err := parsePlexSessions([]byte(raw)); err == nil {
			t.Fatal("ambiguous sessions", raw)
		}
	}
	raw, _ := json.Marshal(map[string]any{"MediaContainer": map[string]any{"size": 0, "Metadata": []any{}}})
	if rows, err := parsePlexSessions(raw); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
