package iyuu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMatchingWireProtocol(t *testing.T) {
	a, b, target := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	hashes := []string{b, a}
	raw := fmt.Sprintf(`["%s","%s"]`, a, b)
	clients := Client{Token: "fixture", Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "2025.iyuu.cn" || r.Header.Get("token") != "fixture" || r.Method != "POST" {
			t.Fatal(r.URL)
		}
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["hash"] != raw || in["sha1"] != Digest(raw) || in["version"] != "8.2.0" || in["timestamp"] != float64(17) || in["sid_sha1"] != a {
			t.Fatal(in)
		}
		out := fmt.Sprintf(`{"code":0,"data":{"%s":{"torrent":{"sid":"7","torrent_id":32,"info_hash":"%s"}}}}`, a, target)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(out))}, nil
	})}
	result, err := clients.Match(t.Context(), hashes, a, time.Unix(17, 0))
	if err != nil || len(result[a]) != 1 || result[a][0].Hash != target || !slices.Equal(hashes, []string{b, a}) {
		t.Fatal(result, err)
	}
	if _, err := clients.Match(t.Context(), []string{a, a}, a, time.Now()); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
}
func TestUnexpectedRemoteData(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, body := range []string{`{}`, `{"code":0,"data":null}`, fmt.Sprintf(`{"code":0,"data":{"%s":{"torrent":[]}}}`, b), fmt.Sprintf(`{"code":0,"data":{"%s":{"torrent":[{"sid":0}]}}}`, a)} {
		c := Client{Token: "fixture", Transport: transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if _, err := c.Match(context.Background(), []string{a}, b, time.Now()); !errors.Is(err, ErrResponse) {
			t.Fatal(body, err)
		}
	}
}
func TestIYUUSettingsRetainLegacyIDsAndCaches(t *testing.T) {
	c, err := Read(`{"downloaders":[1,"2","1"],"sites":["3"],"nolabels":"a, b","error_caches":["dead"],"permanent_error_caches":["abcd"],"success_caches":["done"]}`)
	if err != nil || !slices.Equal(c.Downloaders, []string{"1", "2"}) || !slices.Equal(c.Excluded, []string{"a", "b"}) || !c.Permanent["abcd"] || !c.Failed["dead"] || !c.Success["done"] {
		t.Fatal(c, err)
	}
	for _, raw := range []string{`{"sites":"all"}`, `{"sites":[-1]}`, `{"downloaders":["01"]}`, `{"token":"a b"}`, `{"success_caches":{}}`} {
		if _, err := Read(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestBindingRejectsUnconfirmedResponsesAndRedirects(t *testing.T) {
	for _, v := range []struct {
		status int
		body   string
		want   error
	}{
		{200, `{"code":0,"data":[]}`, nil},
		{200, `{"code":0,"data":[{}]}`, ErrResponse},
		{200, `{"code":1,"data":[]}`, ErrRejected},
		{200, `{"code":0,"data":null}`, ErrResponse},
		{307, "", ErrResponse},
	} {
		calls := 0
		c := Client{Token: "fixture", Transport: transport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "2025.iyuu.cn" {
				t.Fatal("redirected request")
			}
			return &http.Response{StatusCode: v.status, Header: http.Header{"Location": {"https://elsewhere.invalid/"}}, Body: io.NopCloser(strings.NewReader(v.body))}, nil
		})}
		err := c.Bind(t.Context(), Partner{ID: "1", Site: "site"}, "2", "test-passkey")
		if !errors.Is(err, v.want) || calls != 1 {
			t.Fatal(err, calls)
		}
	}
}
