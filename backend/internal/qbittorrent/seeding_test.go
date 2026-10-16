package qbittorrent

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSeedingEvidenceNeverInfersElapsedFromCompletionTime(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, fields string
		complete     int
		counters     bool
		fail         bool
	}{
		{"complete", `"state":"uploading","progress":1,"amount_left":0,"seeding_time":900,"ratio":1.2`, 1, true, false},
		{"paused complete", `"state":"stoppedUP","progress":1,"amount_left":0,"seeding_time":900,"ratio":1.2`, 1, true, false},
		{"checking", `"state":"checkingUP","progress":1,"amount_left":0,"seeding_time":900,"ratio":1.2`, 0, true, false},
		{"unknown state", `"state":"future","progress":1,"amount_left":0`, -1, false, false},
		{"missing counters", `"state":"uploading","progress":1,"amount_left":0,"completion_on":1`, 1, false, false},
		{"negative sentinels", `"state":"uploading","progress":1,"amount_left":0,"seeding_time":-1,"ratio":-1`, 1, false, false},
		{"missing progress", `"state":"uploading"`, -1, false, false},
		{"malformed", `"state":"uploading","seeding_time":"900"`, -1, false, true},
		{"negative left", `"state":"uploading","progress":1,"amount_left":-1`, -1, false, true},
		{"bad progress", `"state":"uploading","progress":1.1,"amount_left":0`, -1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c, err := New("qb.test", "8080", "user", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if strings.HasSuffix(r.URL.Path, "auth/login") {
					return reply(200, "Ok."), nil
				}
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "torrents/info") || r.URL.Query().Get("hashes") != hash || r.URL.Query().Get("filter") != "all" {
					t.Fatal(r.URL)
				}
				return reply(200, `[{"hash":"`+hash+`",`+tc.fields+`}]`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			e, err := c.SeedingEvidence(context.Background(), hash)
			if (err != nil) != tc.fail {
				t.Fatal(e, err)
			}
			if tc.fail {
				return
			}
			if calls != 2 {
				t.Fatal(calls)
			}
			if tc.complete == -1 {
				if e.Complete != nil {
					t.Fatal(e)
				}
			} else if e.Complete == nil || *e.Complete != (tc.complete == 1) {
				t.Fatal(e)
			}
			if tc.counters {
				if e.Seconds == nil || *e.Seconds != 900 || e.Ratio == nil || *e.Ratio != 1.2 {
					t.Fatal(e)
				}
			} else if e.Seconds != nil || e.Ratio != nil {
				t.Fatal("invented counters", e)
			}
		})
	}
}

func TestSeedingEvidenceRejectsAmbiguousTaskIdentity(t *testing.T) {
	hash := strings.Repeat("b", 40)
	for _, body := range []string{`[]`, `null`, `{}`, `[{"hash":"` + strings.Repeat("c", 40) + `","state":"uploading"}]`, `[{"hash":"` + hash + `","state":"uploading"},{"hash":"` + hash + `","state":"uploading"}]`} {
		c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "auth/login") {
				return reply(200, "Ok."), nil
			}
			return reply(200, body), nil
		}))
		if _, err := c.SeedingEvidence(context.Background(), hash); err == nil {
			t.Fatal(body)
		}
	}
}
