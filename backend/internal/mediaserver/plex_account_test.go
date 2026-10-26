package mediaserver

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestPlexAccountDiscoveryKeepsAccountCredentialsAtFixedPlexHTTPSAndUsesSelectedResourceToken(t *testing.T) {
	signins, discoveries, serverCalls := 0, 0, 0
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "plex.tv":
			if r.URL.Scheme != "https" || r.URL.Query().Get("X-Plex-Token") != "" || r.Header.Get("X-Plex-Client-Identifier") != strings.Repeat("a", 32) {
				t.Fatal("account credential scope", r.URL)
			}
			if r.URL.Path == "/users/sign_in.json" {
				signins++
				if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("user[login]") != "user@example.test" || r.Form.Get("user[password]") != "private-password" {
					t.Fatal("legacy authentication form")
				}
				return reply(201, `{"user":{"authentication_token":"private-account-token"}}`), nil
			}
			discoveries++
			if r.Method != "GET" || r.URL.Path != "/api/resources" || r.Header.Get("X-Plex-Token") != "private-account-token" {
				t.Fatal("resource discovery scope")
			}
			return reply(200, `<MediaContainer size="2"><Device name="Other" provides="server" clientIdentifier="other"><Connection protocol="https" uri="https://unselected.test"/></Device><Device name="Selected" provides="server" clientIdentifier="server-1" accessToken="private-server-token"><Connection protocol="https" uri="https://selected.test/prefix" local="1"/></Device></MediaContainer>`), nil
		case "selected.test":
			serverCalls++
			if r.Header.Get("X-Plex-Token") != "private-server-token" || r.Header.Get("Authorization") != "" || strings.Contains(r.URL.String(), "private") {
				t.Fatal("account token or password forwarded to server")
			}
			if strings.HasSuffix(r.URL.Path, "/library/sections") {
				return reply(200, `{"MediaContainer":{"size":0,"Directory":[]}}`), nil
			}
			return reply(200, `{"MediaContainer":{"machineIdentifier":"server-1"}}`), nil
		default:
			t.Fatal("unselected discovery destination", r.URL)
			return nil, ErrConnection
		}
	})
	client, err := ResolvePlexAccount(t.Context(), "user@example.test", "private-password", "Selected", strings.Repeat("a", 32), false, transport)
	if err != nil || client == nil || signins != 1 || discoveries != 1 || serverCalls != 1 {
		t.Fatal(err, signins, discoveries, serverCalls)
	}
	p, err := client.PlanRefresh(t.Context(), nil, true)
	if err != nil || p.ServerID != "server-1" || len(p.Targets) != 0 {
		t.Fatal(p, err)
	}
}
func TestPlexAccountDiscoveryRejectsAmbiguityMissingResourceTokensAndUnapprovedHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		allowHTTP, success bool
	}{
		{"missing-token", `<MediaContainer><Device name="Selected" provides="server" clientIdentifier="server-1"><Connection protocol="https" uri="https://selected.test"/></Device></MediaContainer>`, false, false},
		{"ambiguous", `<MediaContainer size="2"><Device name="Selected" provides="server"/><Device name="selected" provides="server"/></MediaContainer>`, false, false},
		{"truncated-count", `<MediaContainer size="2"><Device name="Selected" provides="server"/></MediaContainer>`, false, false},
		{"http-not-approved", `<MediaContainer><Device name="Selected" provides="server" clientIdentifier="server-1" accessToken="resource-token"><Connection protocol="http" uri="http://selected.test"/></Device></MediaContainer>`, false, false},
		{"http-explicit", `<MediaContainer><Device name="Selected" provides="server" clientIdentifier="server-1" accessToken="resource-token"><Connection protocol="http" uri="http://selected.test"/></Device></MediaContainer>`, true, true},
		{"protocol-mismatch", `<MediaContainer><Device name="Selected" provides="server" clientIdentifier="server-1" accessToken="resource-token"><Connection protocol="https" uri="http://selected.test"/></Device></MediaContainer>`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverCalls := 0
			client, err := ResolvePlexAccount(t.Context(), "user", "password", "Selected", strings.Repeat("a", 32), tc.allowHTTP, transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "plex.tv" {
					if r.URL.Path == "/users/sign_in.json" {
						return reply(200, `{"user":{"authToken":"account-token"}}`), nil
					}
					return reply(200, tc.body), nil
				}
				if !tc.allowHTTP {
					t.Fatal("server token sent over unapproved HTTP")
				}
				serverCalls++
				if r.URL.Host != "selected.test" || r.Header.Get("X-Plex-Token") != "resource-token" {
					t.Fatal("resource scope")
				}
				return reply(200, `{"MediaContainer":{"machineIdentifier":"server-1"}}`), nil
			}))
			if tc.success && (err != nil || client == nil || serverCalls != 1) {
				t.Fatal(err, serverCalls)
			}
			if !tc.success && (err == nil || client != nil || serverCalls != 0) {
				t.Fatal(err, serverCalls)
			}
		})
	}
}
func TestPlexAccountLoginDoesNotFollowRedirectsRetryOrExposePrivateErrors(t *testing.T) {
	for _, code := range []int{302, 401, 403, 500} {
		calls := 0
		_, err := ResolvePlexAccount(t.Context(), "user", "private-password", "Selected", strings.Repeat("a", 32), false, transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "plex.tv" {
				t.Fatal("authentication redirected")
			}
			resp := reply(code, "private-account-failure")
			resp.Header.Set("Location", "https://evil.test")
			return resp, nil
		}))
		if err == nil || calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatal(err, calls)
		}
		if code == 401 && !errors.Is(err, ErrAuthentication) {
			t.Fatal(err)
		}
	}
}
