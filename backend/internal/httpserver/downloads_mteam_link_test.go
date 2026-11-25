package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func TestMTeamManualSubscriptionAndRSSLinks(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	meta, _ := torrentmeta.Parse(testTorrent)
	f.hash = meta.InfoHash
	prev := f.transport
	tokens, gets := 0, 0
	rssPhase := false
	rssAdds := 0
	const direct = "https://api.m-team.cc/api/rss/dl?ticket=fixture%2Fvalue"
	const detail = "https://kp.m-team.cc/detail/77"
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.m-team.cc" {
			if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.UserAgent() != "Fixture Agent" {
				t.Fatal("unexpected M-Team headers")
			}
			switch r.URL.Path {
			case "/api/torrent/genDlToken":
				if r.Method != "POST" || r.Header.Get("x-api-key") != "fixture-key" || r.ParseForm() != nil || r.Form.Get("id") != "77" {
					t.Fatal("invalid token request")
				}
				tokens++
				return jsonResponse(r, `{"code":0,"data":"`+direct+`"}`), nil
			case "/api/rss/dl":
				if r.Method != "GET" || r.URL.String() != direct || r.Header.Get("x-api-key") != "" {
					t.Fatal("invalid signed fetch")
				}
				gets++
				return jsonResponse(r, string(testTorrent)), nil
			}
		}
		if strings.Contains(r.URL.Host, "m-team") {
			t.Fatal("unexpected site request")
		}
		if rssPhase && r.URL.Hostname() == "client.local" && strings.HasSuffix(r.URL.Path, "/torrents/add") {
			var claims int
			if e := f.db.QueryRow("SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE TASK_ID=71 AND STATE='pending'").Scan(&claims); e != nil || claims != 1 {
				t.Fatal("missing RSS reservation", claims, e)
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(data, testTorrent) {
				t.Fatal("incorrect RSS payload", err)
			}
			rssAdds++
			return jsonResponse(r, "Ok."), nil
		}
		return prev.RoundTrip(r)
	})
	f.rebuild(t)
	store, err := siteconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	site, err := store.Upsert(t.Context(), siteconfig.Site{Name: "Fixture", SignURL: "https://kp.m-team.cc/login", APIKey: "fixture-key", Cookie: "unused-cookie", Note: `{"ua":"Fixture Agent"}`})
	if err != nil {
		t.Fatal(err)
	}
	manual := func(raw string) int {
		data, _ := json.Marshal(map[string]string{"siteId": text(site.ID), "url": raw})
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/link", f.token, string(data))
		if strings.Contains(w.Body.String(), "fixture-key") || strings.Contains(w.Body.String(), "ticket=") {
			t.Fatal("credential in response")
		}
		return w.Code
	}
	if manual(detail) != 200 || manual(direct) != 200 || f.adds != 1 || tokens != 1 || gets != 2 {
		t.Fatal(f.adds, tokens, gets)
	}
	if manual("https://api.m-team.io/api/rss/dl?ticket=other") != 400 {
		t.Fatal("cross-site signed URL accepted")
	}
	candidate := subscriptionCandidate{site: &site, resource: externalindexer.Resource{DownloadURL: direct}}
	_, content, err := f.runner.download.service.prepareSubscriptionTorrent(t.Context(), candidate)
	if err != nil || !bytes.Equal(content, testTorrent) || tokens != 1 || gets != 3 {
		t.Fatal(tokens, gets, err)
	}
	candidate.resource.DownloadURL = "https://xp.m-team.cc/detail/77"
	if _, _, e := f.runner.download.service.prepareSubscriptionTorrent(t.Context(), candidate); !errors.Is(e, errSiteLinkNotAllowed) {
		t.Fatal(e)
	}
	if _, err = f.db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,ADDRESS,PARSER,USES,DOWNLOAD_SETTING) VALUES (71,'Fixture RSS','[]','[]','D',-2)`); err != nil {
		t.Fatal(err)
	}
	rssPhase = true
	rows, _ := json.Marshal([]rssDownloadArticle{{Title: "Fixture", Enclosure: direct}})
	for i := 0; i < 2; i++ {
		w := performFormRequest(f.handler, "/api/v1/rss/item/download", f.token, url.Values{"taskid": {"71"}, "articles": {string(rows)}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":0`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if f.adds != 1 || rssAdds != 1 || tokens != 1 || gets != 4 {
		t.Fatal(f.adds, rssAdds, tokens, gets)
	}
}

func TestMTeamCredentialOriginAndSignedRSSAlias(t *testing.T) {
	const raw = "https://api.m-team.cc/api/rss/dl?ticket=sample"
	first := siteconfig.Site{ID: 1, SignURL: "https://kp.m-team.cc/login"}
	second := siteconfig.Site{ID: 2, SignURL: "https://xp.m-team.cc/login", RSSURL: "https://api.m-team.cc/api/rss/feed"}
	chosen, err := mteamDownloadSite([]siteconfig.Site{first, second}, raw)
	if err != nil || chosen.ID != second.ID {
		t.Fatal(chosen.ID, err)
	}
	second.RSSURL = ""
	if _, err = mteamDownloadSite([]siteconfig.Site{first, second}, raw); err == nil {
		t.Fatal("ambiguous API alias accepted")
	}
	foreign := siteconfig.Site{SignURL: "https://other.example/", RSSURL: "https://kp.m-team.cc/rss", APIKey: "other-site-key"}
	if siteMatchesMTeamDownload(foreign, "https://kp.m-team.cc/detail/42") || !siteMatchesMTeamDownload(foreign, raw) {
		t.Fatal("RSS configuration expanded API-key scope")
	}
}
