package httpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrganizationEpisodeCompatibilityWorksInLiteralModesAndSavedRules(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, _ := manualIdentityFixture(t)
			input.Mode, input.TargetID = mode, subscriptionResourceKey("target:"+target+":TV")
			input.EpisodeSelection = batchEpisodeRule()
			input.EpisodeSelection.EpisodeFormat = "{{{team}}}.{year:d}.WRONG.{ep:4}.{team}.mkv"
			for _, name := range []string{"{Group}.2026.Wrong.0011.group.mkv", "{Group}.2026.Wrong.0011.group.en.srt", "{Group}.2026.Wrong.0012.Other.mkv"} {
				organizationFile(t, source, name)
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 3 {
				t.Fatal(status, body)
			}
			available, filtered := 0, 0
			for _, item := range plan.Items {
				if item.Status == "available" {
					available++
					if item.SeasonEpisode != "S01E01" {
						t.Fatal(item)
					}
				} else if item.Status == "filtered" {
					filtered++
				} else {
					t.Fatal(item)
				}
			}
			if available != 2 || filtered != 1 {
				t.Fatal(available, filtered)
			}
			oldFormat := input.EpisodeSelection.EpisodeFormat
			input.EpisodeSelection.EpisodeFormat = "{{{team}}}.{year:d}.WRONG.{ep}.{team}.mkv"
			status, body = batchCreateResponse(f, input, plan.Fingerprint)
			if status != 409 {
				t.Fatal("same result but changed template accepted", status, body)
			}
			input.EpisodeSelection.EpisodeFormat = oldFormat
			job := organizationCreateJob(t, f, input, plan)
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" || done.EpisodeSelection == nil || done.EpisodeSelection.EpisodeFormat != oldFormat {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "{Group}.2026.Wrong.0012.Other.mkv"), "original-media")
		})
	}
}

func TestOrganizationEpisodeCompatibilityManualFormatAndMatchingBudget(t *testing.T) {
	f, source, target, input, paths := manualIdentityFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "{Group}.2026.Wrong.0011.group.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	selection := assignmentFor(initial.Items[0], "tv", "200")
	season := 1
	selection.Season, selection.EpisodeFormat, selection.EpisodeOffset = &season, "{{{team}}}.{year:d}.WRONG.{ep:4}.{team}.mkv", -10
	input.Assignments = []organizationMediaAssignment{selection}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].SeasonEpisode != "S01E01" {
		t.Fatal(status, body)
	}
	selection.EpisodeFormat = "{team:__import__}.{ep}.mkv"
	input.Assignments = []organizationMediaAssignment{selection}
	*paths = nil
	status, _, body = organizationPreview(t, f, input)
	if status != 422 || len(*paths) != 0 {
		t.Fatal(status, body, *paths)
	}

	budget, bs, bt, bi, bp := manualIdentityFixture(t)
	bi.TargetID, bi.EpisodeSelection = subscriptionResourceKey("target:"+bt+":TV"), batchEpisodeRule()
	bi.EpisodeSelection.EpisodeFormat = "{a}{b}{c}{d}{e}{f}{g}{a}.{ep}.mkv"
	organizationFile(t, bs, strings.Repeat("a", 150)+"z.0011.mkv")
	started := time.Now()
	status, plan, body = organizationPreview(t, budget, bi)
	if status != 200 || plan.Items[0].Status != "blocked" || !strings.Contains(plan.Items[0].Reason, "budget") || len(*bp) != 0 || time.Since(started) > 3*time.Second {
		t.Fatal("budget failure became filtered or executable", status, body, *bp, time.Since(started))
	}
	status, body = batchCreateResponse(budget, bi, plan.Fingerprint)
	if status != 422 {
		t.Fatal("saved timed-out match", status, body)
	}
}
