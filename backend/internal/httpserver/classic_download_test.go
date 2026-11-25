package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func TestClassicDownloadCommitsHistoryAndNoticeOnce(t *testing.T) {
	var calls atomic.Int32
	var lose atomic.Bool
	const gid = "0123456789abcdef"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID, Method string
			Params     []any
		}
		if json.NewDecoder(r.Body).Decode(&rpc) != nil || rpc.Method != "aria2.addUri" || len(rpc.Params) != 2 {
			t.Error("wrong unauthenticated Aria2 submission")
			return
		}
		calls.Add(1)
		if lose.Load() {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": gid})
	}))
	defer remote.Close()
	endpoint, _ := url.Parse(remote.URL)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "user.db")
	path := filepath.Join(dir, "config.yaml")
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg, _ := json.Marshal(map[string]string{"host": "http://" + endpoint.Hostname(), "port": endpoint.Port()})
	item, err := store.Upsert(t.Context(), downloaderconfig.Downloader{Name: "Aria", Type: "aria2", Enabled: 1, Config: string(cfg), DownloadDir: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	q := classicRequest{Downloader: item.ID, Channel: "WEB", Sender: "operator", Source: "WEB", Title: "Movie", Config: cfg, Directories: json.RawMessage("[]"), Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40), Notice: downloadhistory.Notice{Title: "Movie", Source: "WEB", Promotion: "免费"}}
	submit := func() map[string]any {
		t.Helper()
		b, _ := json.Marshal(q)
		var out bytes.Buffer
		if err := ClassicDownloadCommand(t.Context(), path, "submit", bytes.NewReader(b), &out); err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if json.Unmarshal(out.Bytes(), &reply) != nil {
			t.Fatal("invalid reply")
		}
		return reply
	}
	if r := submit(); r["task"] != gid || r["reused"] != false {
		t.Fatal(r)
	}
	if r := submit(); r["task"] != gid || r["reused"] != true {
		t.Fatal(r)
	}
	q.Sender = "other"
	if r := submit(); r["pending"] != true {
		t.Fatal(r)
	}
	q.Sender = "operator"
	q.Config = json.RawMessage("{}")
	if r := submit(); r["rejected"] != true {
		t.Fatal(r)
	}
	q.Config = cfg
	q.ClientType = "qbittorrent"
	if r := submit(); r["rejected"] != true {
		t.Fatal("client type changed", r)
	}
	q.ClientType = "aria2"
	q.Magnet = "magnet:?xt=urn:btih:" + strings.Repeat("b", 40)
	lose.Store(true)
	if r := submit(); r["pending"] != true {
		t.Fatal(r)
	}
	if r := submit(); r["pending"] != true {
		t.Fatal(r)
	}
	if calls.Load() != 2 {
		t.Fatal("remote submissions", calls.Load())
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"SELECT count(*) FROM DOWNLOAD_HISTORY WHERE DOWNLOAD_ID='" + gid + "'", "SELECT count(*) FROM GO_BUSINESS_EVENTS WHERE KIND='download_start' AND REFERENCE LIKE 'classic-download:%'", "SELECT count(*) FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE STATE='pending'"} {
		var n int
		if err := db.QueryRow(stmt).Scan(&n); err != nil || n != 1 {
			t.Fatal(stmt, n, err)
		}
	}
}

func TestClassicAria2HybridMagnet(t *testing.T) {
	v1 := "urn:btih:" + strings.Repeat("a", 40)
	v2 := "urn:btmh:1220" + strings.Repeat("b", 64)
	for _, topics := range [][]string{{v1, v2}, {v2, v1}, {v1, v1}, {v2}} {
		q := url.Values{"xt": topics, "tr": {"https://fixture.test/a?k=1", "udp://fixture.test:42/a"}}
		got := classicAria2Magnet("magnet:?" + q.Encode())
		if len(topics) == 1 || topics[0] == topics[1] {
			if got != "" {
				t.Fatal("ambiguous identity accepted")
			}
			continue
		}
		u, err := url.Parse(got)
		if err != nil || u.Query().Get("xt") != v1 || len(u.Query()["tr"]) != 2 {
			t.Fatal(got, err)
		}
	}
}

func TestClassicMagnetKeepsClientIdentities(t *testing.T) {
	v1 := "urn:btih:" + strings.Repeat("a", 40)
	v2 := "urn:btmh:1220" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		topics []string
		qb, tr bool
		hash   string
	}{
		{[]string{v2, v1}, true, true, strings.Repeat("a", 40)},
		{[]string{v1, v2}, true, true, strings.Repeat("a", 40)},
		{[]string{v2}, true, false, strings.Repeat("b", 64)},
		{[]string{v1, v1}, false, false, ""},
		{[]string{v2, v2}, false, false, ""},
	} {
		raw := "magnet:?" + url.Values{"xt": tc.topics, "tr": {"https://fixture.invalid/tracker?k=v"}}.Encode()
		for _, qb := range []bool{true, false} {
			got := classicTorrentMagnet(raw, qb)
			accepted := tc.tr || qb && tc.qb
			if (got != "") != accepted || accepted && (got != raw || magnetInfoHash(got) != tc.hash) {
				t.Fatal(tc, qb, got)
			}
		}
	}
}

func TestClassicHybridHonorsWireIDSeedingPolicy(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	s := f.runner.download.service
	item, err := f.store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	v2 := strings.Repeat("d", 64)
	_, digest, err := f.store.SeedingPolicies(t.Context(), item.ID, v2[:40])
	if err != nil {
		t.Fatal(err)
	}
	seconds := int64(86400)
	_, err = f.store.BindSeedingPolicy(t.Context(), downloaderconfig.SeedingPolicy{DownloaderID: item.ID, DownloaderType: item.Type, DownloaderName: item.Name, ConfigDigest: bus.Hash(item), InfoHash: v2[:40], SourceDigest: bus.Hash("fixture"), Requirements: seeding.Requirements{Seconds: &seconds}, Actor: "0:admin"}, digest)
	if err != nil {
		t.Fatal(err)
	}
	result := classicDownloadResult{history: downloaderconfig.ClassicAcknowledgement{V1Hash: f.hash, V2Hash: v2}}
	magnet := "magnet:?xt=urn:btih:" + f.hash + "&xt=urn:btmh:1220" + v2
	_, _, err = s.submitWithReceipt(t.Context(), "classic:fixture", magnet, nil, &item, downloadAddOptions{}, "fixture", "WEB", nil, nil, &result)
	if !errors.Is(err, errPTAdmissionBlocked) || f.adds != 0 {
		t.Fatal(err, f.adds)
	}
}
