package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTNodeCommandBrowsesResourcePage(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" || r.Header.Get("Cookie") != "session=fixture" || r.UserAgent() != "fixture-agent" {
			t.Errorf("unexpected host or request headers")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/":
			io.WriteString(w, `<meta name="x-csrf-token" content="fixture-token">`)
		case r.Method == "POST" && r.URL.Path == "/api/torrent/advancedSearch":
			var payload struct {
				Keyword    *string
				Size, Page int
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Keyword == nil || *payload.Keyword != "" || payload.Page != 3 || payload.Size != 150 || r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
				t.Errorf("unexpected browse request: %+v", payload)
			}
			io.WriteString(w, `{"data":{"torrents":[{"id":12,"title":"Fixture","size":25,"complete":"17","upload_time":"1700000000"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()

	transport := site.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "example.com:443" {
			return nil, fmt.Errorf("unexpected address %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
	}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	var output bytes.Buffer
	request := `{"id":"fixture","domain":"https://example.com/","cookie":"session=fixture","ua":"fixture-agent","page":2,"limit":150}`
	if err := ResourcePageCommand(context.Background(), "tnode-resources", strings.NewReader(request), &output); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("unexpected output %s", output.String())
	}
	row := rows[0]
	if row["indexer"] != "fixture" || row["grabs"] != "17" || row["pubdate"] != time.Unix(1700000000, 0).Format("2006-01-02 15:04:05") || row["page_url"] != "https://example.com/torrent/info/12" {
		t.Fatal(row)
	}
	for _, invalid := range []string{request + "{}", strings.Replace(request, `"limit":150`, `"proxy":"broken","limit":150`, 1)} {
		output.Reset()
		if err := ResourcePageCommand(context.Background(), "tnode-resources", strings.NewReader(invalid), &output); err != ErrConfig || output.Len() != 0 {
			t.Fatal(err, output.String())
		}
	}
}
