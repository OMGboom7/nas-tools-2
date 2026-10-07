package qbittorrent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHasTorrentIncludesAllStatesAndRequiresExactHash(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, test := range []struct {
		body           string
		found, invalid bool
	}{
		{`[{"hash":"` + hash + `","state":"pausedUP"}]`, true, false},
		{`[{"hash":"` + strings.ToUpper(hash) + `","state":"uploading"}]`, true, false},
		{`[]`, false, false}, {`null`, false, true}, {`{}`, false, true}, {`[{}]`, false, true},
		{`[{"hash":"1123456789abcdef0123456789abcdef01234567"}]`, false, true},
		{`[{"hash":"` + hash + `"},{"hash":"` + hash + `"}]`, false, true},
	} {
		t.Run(test.body, func(t *testing.T) {
			client, err := New("qb.example", "8080", "admin", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/auth/login") {
					return reply(200, "Ok."), nil
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/torrents/info" || r.URL.Query().Get("filter") != "all" || r.URL.Query().Get("hashes") != hash {
					t.Fatal("unsafe verification request", r.URL, r.Method)
				}
				return reply(200, test.body), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			found, err := client.HasTorrent(context.Background(), hash)
			if found != test.found || (err != nil) != test.invalid {
				t.Fatal(found, err)
			}
		})
	}
}
