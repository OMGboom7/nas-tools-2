package externalindexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type handlerTransport func(http.ResponseWriter, *http.Request)

func (handler handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	handler(response, request)
	result := response.Result()
	result.Request = request
	return result, nil
}

func TestDiscoveryPreservesLegacyIdentityAndCredentials(t *testing.T) {
	for _, kind := range []string{"Jackett", "Prowlarr"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Api-Key") != "secret-api-key" || strings.Contains(r.URL.String(), "secret") {
					t.Error("credentials missing or in URL")
				}
				switch r.URL.Path {
				case "/prefix/UI/Dashboard":
					if r.Method != "POST" || r.ParseForm() != nil || r.PostForm.Get("password") != "secret-password" {
						t.Error("bad login")
					}
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
					w.Header().Set("Location", "/prefix/")
					w.WriteHeader(302)
				case "/prefix/api/v2.0/indexers":
					if r.URL.Query().Get("configured") != "true" || !strings.Contains(r.Header.Get("Cookie"), "session=authenticated") {
						t.Error("missing selection/cookie")
					}
					fmt.Fprint(w, `[{"id":"tracker-id","name":"Private tracker","type":"private"}]`)
				case "/prefix/api/v1/indexerstats":
					fmt.Fprint(w, `{"indexers":[{"indexerId":7,"indexerName":"中文 tracker"}]}`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(404)
				}
			})
			cfg := Config{Kind: kind, Host: "http://indexer.local/prefix/", APIKey: "secret-api-key"}
			if kind == "Jackett" {
				cfg.Password = "secret-password"
			}
			items, err := Discover(t.Context(), cfg, transport)
			if err != nil || len(items) != 1 {
				t.Fatalf("items=%+v err=%v", items, err)
			}
			if kind == "Jackett" && (items[0].ID != "tracker-id-jackett" || items[0].Public || calls != 2) {
				t.Fatal(items, calls)
			}
			if kind == "Prowlarr" && (items[0].ID != "中文 tracker-prowlarr" || !items[0].Public || calls != 1) {
				t.Fatal(items, calls)
			}
			if strings.Contains(fmt.Sprint(items), "secret") {
				t.Fatal("credentials leaked")
			}
		})
	}
}

func TestDiscoveryRejectsMalformedResponseAndRedirects(t *testing.T) {
	for _, fixture := range []struct {
		kind, body string
		status     int
	}{
		{"Jackett", `null`, 200}, {"Jackett", `<html>login</html>`, 200}, {"Jackett", `[{"id":"../evil","name":"x","type":"public"}]`, 200},
		{"Jackett", `[{"id":"x","name":"x","type":"public"},{"id":"x","name":"y","type":"public"}]`, 200},
		{"Prowlarr", `{"indexers":[{"indexerId":0,"indexerName":"x"}]}`, 200}, {"Prowlarr", `{"indexers":[{"indexerId":1,"indexerName":"x"},{"indexerId":2,"indexerName":"x"}]}`, 200},
		{"Prowlarr", `{}`, 200}, {"Prowlarr", `[]`, 200}, {"Jackett", `[]`, 401}, {"Jackett", `[]`, 302}, {"Jackett", strings.Repeat("x", maxResponse+1), 200},
	} {
		transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://never-follow.invalid/secret")
			w.WriteHeader(fixture.status)
			fmt.Fprint(w, fixture.body)
		})
		_, err := Discover(t.Context(), Config{Kind: fixture.kind, Host: "http://indexer.local", APIKey: "secret"}, transport)
		if !errors.Is(err, ErrResponse) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("kind=%s status=%d err=%v", fixture.kind, fixture.status, err)
		}
	}
}

func TestDiscoveryConfigurationAndCancellation(t *testing.T) {
	for _, host := range []string{"file:///tmp/data", "http://user:pass@example.com", "http://example.com/?key=secret", "http://example.com/#secret"} {
		if _, err := Discover(t.Context(), Config{Kind: "Jackett", Host: host, APIKey: "key"}, nil); !errors.Is(err, ErrConfig) {
			t.Fatalf("host=%s err=%v", host, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, Config{Kind: "Jackett", Host: "http://localhost:9117", APIKey: "key"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestJackettLoginDoesNotFollowRedirectOrForwardSecrets(t *testing.T) {
	calls := 0
	transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Hostname() != "indexer.local" {
			t.Fatal("credentials forwarded")
		}
		w.Header().Set("Location", "https://other.invalid/collect")
		w.WriteHeader(302)
	})
	_, err := Discover(t.Context(), Config{Kind: "Jackett", Host: "http://indexer.local", APIKey: "secret-key", Password: "secret-password"}, transport)
	if !errors.Is(err, ErrResponse) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestEmptyIndexerListIsAuthoritative(t *testing.T) {
	for kind, body := range map[string]string{"Jackett": "[]", "Prowlarr": `{"indexers":[]}`} {
		transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		items, err := Discover(t.Context(), Config{Kind: kind, Host: "http://indexer.local", APIKey: "key"}, transport)
		if err != nil || items == nil || len(items) != 0 {
			t.Fatalf("kind=%s items=%v err=%v", kind, items, err)
		}
	}
}
