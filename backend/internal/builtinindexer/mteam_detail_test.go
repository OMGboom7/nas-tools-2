package builtinindexer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
)

func TestMTeamTorrentDetail(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		title, status                string
		free, double, invalid, timed bool
	}{
		{"Film", `{"discount":"_2X_FREE","seeders":"5","mallSingleFree":false,"toppingEndTime":"2026-05-02 08:00:00"}`, true, true, false, false},
		{"Film", `{"discount":"NORMAL","seeders":5,"mallSingleFree":{"endDate":"2026-05-01 08:01:00"}}`, true, false, false, true},
		{"Film", `{"discount":"NORMAL","seeders":5,"mallSingleFree":{"endDate":"2026-05-01 07:59:59"}}`, false, false, false, false},
		{"Film", `{"discount":"FREE","seeders":5,"mallSingleFree":{"endDate":"2026-05-01 08:01:00"}}`, true, false, false, false},
		{"Other", `{"discount":"FREE","seeders":5}`, false, false, true, false},
		{"Film", `{"discount":"FREE","seeders":-1}`, false, false, true, false},
	} {
		r := externalindexer.Resource{Title: tc.title, PageURL: "https://kp.m-team.cc/detail/77?from=rss"}
		tr := fakeTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != "POST" || req.URL.String() != "https://api.m-team.cc/api/torrent/detail?id=77" || req.Header.Get("Cookie") != "" || req.Header.Get("X-API-Key") != "key" {
				t.Fatal(req.URL, req.Header)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"0","data":{"id":"77","name":"Film","status":` + tc.status + `}}`))}, nil
		})
		got, end, err := MTeamTorrentDetail(context.Background(), r, "key", "UA", now, tr)
		if tc.invalid {
			if err == nil {
				t.Fatal("accepted invalid detail", tc)
			}
			continue
		}
		if err != nil || got.Seeders == nil || *got.Seeders != 5 || got.HitAndRun != nil || got.Freeleech == nil || *got.Freeleech != tc.free || (*got.UploadFactor == 2) != tc.double || tc.free && end == nil {
			t.Fatal(tc, got, end, err)
		}
		expectedDeadline := ""
		if tc.timed {
			expectedDeadline = time.Date(2026, 5, 1, 0, 1, 0, 0, time.UTC).Local().Format(time.DateTime)
		}
		if got.PromotionEnd != expectedDeadline {
			t.Fatal(tc, got.PromotionEnd, expectedDeadline)
		}
	}
}

func TestMTeamProfileNeedsAccountIdentity(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"data":{"username":"member"}}`, true}, {`{"code":"0","data":{"username":"member"}}`, true},
		{`{"code":1,"data":{"username":"member"}}`, false}, {`{"code":0,"data":[]}`, false},
		{`{"code":0,"data":{"error":"credentials rejected"}}`, false},
	} {
		tr := fakeTransport(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://api.m-team.info/api/member/profile" || req.Method != "POST" || req.Header.Get("Cookie") != "" || req.Header.Get("x-api-key") != "key" {
				t.Fatal(req.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})
		err := CheckMTeamProfile(t.Context(), "https://kp.m-team.info", "key", "UA", tr)
		if (err == nil) != tc.ok {
			t.Fatal(tc, err)
		}
	}
}
