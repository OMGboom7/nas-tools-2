package mediaserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestEmbyAndJellyfinChecksUseProtectedHeader(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			path := "/prefix/System/Info"
			if kind == "emby" {
				path = "/prefix/emby/System/Info"
			}
			client, err := New(kind, "https://media.example/prefix/", "server-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != path || request.Header.Get("X-Emby-Token") != "server-secret" || request.URL.Query().Get("api_key") != "" {
					t.Fatalf("request = %s header=%q", request.URL, request.Header.Get("X-Emby-Token"))
				}
				return reply(200, `{"Id":"server-id","Version":"10.10"}`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPlexCheckAcceptsJSONAndXML(t *testing.T) {
	for _, body := range []string{
		`{"MediaContainer":{"machineIdentifier":"plex-id"}}`,
		`<MediaContainer size="0" machineIdentifier="plex-id"></MediaContainer>`,
	} {
		client, err := New("plex", "http://plex.example:32400", "plex-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://plex.example:32400/" || request.Header.Get("X-Plex-Token") != "plex-secret" || request.Header.Get("X-Plex-Client-Identifier") == "" || request.URL.Query().Get("X-Plex-Token") != "" {
				t.Fatalf("request = %s headers=%v", request.URL, request.Header)
			}
			return reply(200, body), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Check(context.Background()); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
	}
}

func TestCheckRejectsFailuresAndRedirects(t *testing.T) {
	fixtures := []struct {
		code int
		body string
		want error
	}{{401, "secret", ErrAuthentication}, {302, "secret", ErrResponse}, {200, `{}`, ErrResponse}, {200, strings.Repeat("x", (1<<20)+1), ErrResponse}}
	for _, fixture := range fixtures {
		calls := 0
		client, err := New("jellyfin", "media.example", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Hostname() != "media.example" {
				t.Fatal("credential followed redirect")
			}
			response := reply(fixture.code, fixture.body)
			response.Header.Set("Location", "https://evil.example/collect")
			return response, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Check(context.Background()); !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "secret") || calls != 1 {
			t.Fatalf("Check = %v calls=%d", err, calls)
		}
	}
}

func TestConfigurationValidation(t *testing.T) {
	for _, fixture := range []struct{ kind, host, credential string }{
		{"unknown", "media.example", "secret"}, {"emby", "", "secret"}, {"emby", "file:///tmp/media", "secret"},
		{"emby", "http://user:password@media.example", "secret"}, {"plex", "plex.example", ""},
	} {
		if _, err := New(fixture.kind, fixture.host, fixture.credential, nil); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted %#v", fixture)
		}
	}
}

func TestCountsUseAuthenticatedNativeEndpoints(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			client, err := New(kind, "https://media.example/prefix", "server-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "media.example" || request.Header.Get("X-Emby-Token") != "server-secret" || request.URL.Query().Get("api_key") != "" {
					t.Fatalf("unexpected media request: %s", request.URL.String())
				}
				prefix := "/prefix"
				if kind == "emby" {
					prefix += "/emby"
				}
				switch request.URL.Path {
				case prefix + "/Items/Counts":
					return reply(200, `{"MovieCount":1200,"SeriesCount":23,"EpisodeCount":345,"SongCount":6}`), nil
				case prefix + "/Users/Query":
					return reply(200, `{"TotalRecordCount":4}`), nil
				case prefix + "/Users":
					return reply(200, `[{"Id":"1"},{"Id":"2"}]`), nil
				default:
					t.Fatalf("unexpected media path: %s", request.URL.Path)
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			counts, err := client.Counts(context.Background())
			wantUsers := int64(2)
			if kind == "emby" {
				wantUsers = 4
			}
			if err != nil || counts.Movies != 1200 || counts.Series != 23 || counts.Episodes != 345 || counts.Songs != 6 || counts.Users != wantUsers || calls != 2 {
				t.Fatalf("Counts = %#v err=%v calls=%d", counts, err, calls)
			}
		})
	}
}

func TestCountsRejectsFailedAuthenticatedResponse(t *testing.T) {
	client, err := New("emby", "https://media.example", "server-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		return reply(http.StatusUnauthorized, `{"error":"server-secret"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Counts(context.Background()); !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), "server-secret") {
		t.Fatalf("Counts error = %v", err)
	}
}

func TestPlexCountsUseSectionTotals(t *testing.T) {
	for _, format := range []string{"xml", "json"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			client, err := New("plex", "https://plex.example/prefix", "plex-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "plex.example" || request.Header.Get("X-Plex-Token") != "plex-secret" || request.URL.Query().Get("X-Plex-Token") != "" {
					t.Fatalf("unexpected Plex credential handling: %s", request.URL.String())
				}
				if request.URL.Path == "/prefix/library/sections" {
					if format == "json" {
						return reply(200, `{"MediaContainer":{"Directory":[{"key":"1","type":"movie"},{"key":2,"type":"show"},{"key":"3","type":"artist"}]}}`), nil
					}
					return reply(200, `<MediaContainer><Directory key="1" type="movie"/><Directory key="2" type="show"/><Directory key="3" type="artist"/></MediaContainer>`), nil
				}
				if request.URL.Query().Get("X-Plex-Container-Size") != "0" || request.URL.Query().Get("X-Plex-Container-Start") != "0" {
					t.Fatalf("Plex count query not bounded: %s", request.URL.String())
				}
				amount := "0"
				switch request.URL.Path {
				case "/prefix/library/sections/1/all":
					amount = "120"
				case "/prefix/library/sections/2/all":
					if request.URL.Query().Get("type") == "4" {
						amount = "321"
					} else {
						amount = "20"
					}
				case "/prefix/library/sections/3/all":
					amount = "7"
				default:
					t.Fatalf("unexpected Plex path: %s", request.URL.String())
				}
				if format == "json" {
					return reply(200, `{"MediaContainer":{"totalSize":`+amount+`}}`), nil
				}
				return reply(200, `<MediaContainer totalSize="`+amount+`"/>`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			counts, err := client.Counts(context.Background())
			if err != nil || counts != (Counts{Movies: 120, Series: 20, Episodes: 321, Songs: 7, Users: 1}) || calls != 5 {
				t.Fatalf("Plex counts=%#v err=%v calls=%d", counts, err, calls)
			}
		})
	}
}
