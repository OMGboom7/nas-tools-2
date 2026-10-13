package httpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrganizationEpisodeCalendarSelectsOnlyValidDatesInAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, paths := manualIdentityFixture(t)
			input.Mode, input.TargetID, input.EpisodeSelection = mode, subscriptionResourceKey("target:"+target+":TV"), batchEpisodeRule()
			input.EpisodeSelection.EpisodeFormat = "Wrong.{date:ti}.{ep}.mkv"
			for _, name := range []string{"Wrong.2024-02-29.11.mkv", "Wrong.2024-02-29.11.en.srt", "Wrong.2026-02-29.11.mkv", "Wrong.2026-02-29.11.en.srt"} {
				organizationFile(t, source, name)
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 4 {
				t.Fatal(status, body)
			}
			available, blocked := 0, 0
			for _, item := range plan.Items {
				if item.Status == "available" {
					available++
					if item.Year != "2026" || item.SeasonEpisode != "S01E01" {
						t.Fatal("calendar changed verified identity or episodes", item)
					}
				} else if item.Kind == "media" && item.Status == "blocked" && strings.Contains(item.Reason, "date or time") {
					blocked++
				} else if item.Kind != "media" && item.Status == "unmatched" {
					blocked++
				} else {
					t.Fatal(item)
				}
			}
			if available != 2 || blocked != 2 {
				t.Fatal(available, blocked)
			}
			for _, path := range *paths {
				if path != "/3/tv/200" {
					t.Fatal(path)
				}
			}
			job := organizationCreateJob(t, f, input, plan)
			if len(job.Items) != 2 {
				t.Fatal(job)
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "Wrong.2026-02-29.11.mkv"), "original-media")
			assertOrganizationMoveBytes(t, filepath.Join(source, "Wrong.2026-02-29.11.en.srt"), "original-media")
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestOrganizationEpisodeCalendarManualInvalidDatesNeverLookupOrExecute(t *testing.T) {
	f, source, target, input, paths := manualIdentityFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "Wrong.2026-02-29.11.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	selection := assignmentFor(initial.Items[0], "tv", "200")
	season := 1
	selection.Season, selection.EpisodeFormat, selection.EpisodeOffset = &season, "Wrong.{date:ti}.{ep}.mkv", -10
	input.Assignments = []organizationMediaAssignment{selection}
	*paths = nil
	status, _, body = organizationPreview(t, f, input)
	if status != 422 || len(*paths) != 0 {
		t.Fatal(status, body, *paths)
	}
	if items, err := os.ReadDir(target); err != nil || len(items) != 0 {
		t.Fatal("invalid date wrote target", items, err)
	}
}
