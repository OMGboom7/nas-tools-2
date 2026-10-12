package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOrganizationEpisodeFormatExecutesVerifiedOffsetRangeInAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, _ := manualIdentityFixture(t)
			input.Mode = mode
			input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
			for _, name := range []string{"Wrong.第EP11-EP13話.mkv", "Wrong.第EP11-EP13話.en.srt"} {
				organizationFile(t, source, name)
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			status, initial, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			for _, item := range initial.Items {
				if item.Kind == "media" {
					selection := assignmentFor(item, "tv", "200")
					season := 1
					selection.Season, selection.EpisodeFormat, selection.EpisodeOffset = &season, "Wrong.第{ep}話.mkv", -10
					input.Assignments = []organizationMediaAssignment{selection}
				}
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 2 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				if item.Status != "available" || item.SeasonEpisode != "S01E01-E03" || item.EpisodeFormat != input.Assignments[0].EpisodeFormat || item.EpisodeOffset != -10 {
					t.Fatal(item)
				}
			}
			// A different explicit rule producing identical numbers must still
			// invalidate the reviewed provenance fingerprint.
			changed := input
			changed.Assignments = append([]organizationMediaAssignment(nil), input.Assignments...)
			changed.Assignments[0].EpisodeFormat = "{a}.第{ep}話.mkv"
			payload, _ := json.Marshal(struct {
				organizationPlanInput
				Fingerprint string `json:"fingerprint"`
			}{changed, plan.Fingerprint})
			w := nativeJSONRequest(f.handler, http.MethodPost, "/api/v1/organization/jobs", f.token, string(payload))
			if w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
			job := organizationCreateJob(t, f, input, plan)
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				if item.EpisodeOffset != -10 || item.EpisodeFormat == "" {
					t.Fatal("lost rule receipt", item)
				}
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			var count int
			var episode string
			if err := f.db.QueryRow(`SELECT COUNT(*),SEASON_EPISODE FROM TRANSFER_HISTORY`).Scan(&count, &episode); err != nil || count != 1 || episode != "S01E01-E03" {
				t.Fatal(count, episode, err)
			}
		})
	}
}

func TestOrganizationEpisodeFormatRejectsRulesBeforeLookupAndChecksTMDBBounds(t *testing.T) {
	f, source, target, input, paths := manualIdentityFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "Wrong.11.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	base := assignmentFor(initial.Items[0], "tv", "200")
	season := 1
	base.Season, base.EpisodeFormat, base.EpisodeOffset = &season, "Wrong.{ep}.mkv", -10
	for _, kind := range []string{"no-match", "malformed", "both", "missing", "zero", "huge", "movie", "stale"} {
		t.Run(kind, func(t *testing.T) {
			selection := base
			expected := 422
			switch kind {
			case "no-match":
				selection.EpisodeFormat = "Other.{ep}.mkv"
			case "malformed":
				selection.EpisodeFormat = "Wrong.{ep:d}.mkv"
			case "both":
				n := 11
				selection.Episode = &n
			case "missing":
				selection.EpisodeFormat = ""
			case "zero":
				selection.EpisodeOffset = -11
			case "huge":
				selection.EpisodeOffset = 100000
			case "movie":
				selection.Kind = "movie"
				selection.Season = nil
			case "stale":
				selection.Identity = "1:2"
				expected = 409
			}
			input.Assignments = []organizationMediaAssignment{selection}
			*paths = nil
			status, _, body := organizationPreview(t, f, input)
			if status != expected || len(*paths) != 0 {
				t.Fatal(status, body, *paths)
			}
		})
	}
	base.EpisodeOffset = -7 // 11 -> 4, but this verified season has 3 episodes.
	input.Assignments = []organizationMediaAssignment{base}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "blocked" {
		t.Fatal(status, body)
	}
	base.EpisodeFormat = ""
	n := 11
	base.Episode, base.EpisodeOffset = &n, -10
	input.Assignments = []organizationMediaAssignment{base}
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].SeasonEpisode != "S01E01" {
		t.Fatal(status, body)
	}
}
