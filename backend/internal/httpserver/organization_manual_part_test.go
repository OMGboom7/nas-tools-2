package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestOrganizationManualPartCorrectsOrClearsAndPersistsInAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, tc := range []struct{ name, part string }{{"Movie.2026.CD1", "CD02"}, {"Movie.2026.CD1", ""}, {"Movie.2026.CD[01].CD2", "PARTIII"}} {
			t.Run(mode+"/"+tc.part+"/"+tc.name, func(t *testing.T) {
				f, source, target, input, _ := manualIdentityFixture(t)
				input.Mode = mode
				if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title}/{title}-{part}"}); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{tc.name + ".mkv", tc.name + ".en.srt"} {
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
						selection := assignmentFor(item, "movie", "300")
						selection.PartOverride = &tc.part
						input.Assignments = []organizationMediaAssignment{selection}
					}
				}
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 || len(plan.Items) != 2 {
					t.Fatal(status, body)
				}
				for _, item := range plan.Items {
					if item.Status != "available" || item.Part != tc.part || item.PartOverride == nil || *item.PartOverride != tc.part {
						t.Fatal(item)
					}
				}
				job := organizationCreateJob(t, f, input, plan)
				status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
				if status != 200 || done.State != "completed" {
					t.Fatal(status, body)
				}
				for _, item := range done.Items {
					if item.Part != tc.part || item.PartOverride == nil || *item.PartOverride != tc.part {
						t.Fatal("lost explicit part provenance", item)
					}
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
					t.Fatal(count, err)
				}
			})
		}
	}
}

func TestOrganizationManualPartSameValueStillRequiresFreshReviewAndInvalidValuesNeverLookup(t *testing.T) {
	f, source, _, input, paths := manualIdentityFixture(t)
	organizationFile(t, source, "Movie.2026.CD1.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	selection := assignmentFor(initial.Items[0], "movie", "300")
	input.Assignments = []organizationMediaAssignment{selection}
	status, automatic, body := organizationPreview(t, f, input)
	if status != 200 || automatic.Items[0].Part != "CD1" {
		t.Fatal(status, body)
	}
	part := "cd1"
	selection.PartOverride = &part
	input.Assignments = []organizationMediaAssignment{selection}
	status, explicit, body := organizationPreview(t, f, input)
	if status != 200 || explicit.Items[0].PartOverride == nil || *explicit.Items[0].PartOverride != "CD1" || explicit.Fingerprint == automatic.Fingerprint {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, automatic.Fingerprint})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, invalid := range []string{"../CD1", "PART1/CD2", "CUSTOM", "DISC100", "CD1\x00", "CD1.CD2"} {
		selection.PartOverride = &invalid
		input.Assignments = []organizationMediaAssignment{selection}
		*paths = nil
		status, _, body = organizationPreview(t, f, input)
		if status != 422 || len(*paths) != 0 {
			t.Fatal(status, body, *paths)
		}
	}
}

func TestOrganizationManualPartClearDoesNotBypassTargetCollisionAndTVRules(t *testing.T) {
	f, source, _, input, _ := manualIdentityFixture(t)
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title}/{title}-{part}"}); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "Movie.2026.CD1.mkv")
	organizationFile(t, source, "Movie.2026.CD2.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	empty := ""
	for _, item := range initial.Items {
		selection := assignmentFor(item, "movie", "300")
		selection.PartOverride = &empty
		input.Assignments = append(input.Assignments, selection)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status != "conflict" {
			t.Fatal("explicit clear bypassed collision", item)
		}
	}

	tv, tvSource, tvTarget, tvInput, _ := manualIdentityFixture(t)
	tvInput.TargetID = subscriptionResourceKey("target:" + tvTarget + ":TV")
	organizationFile(t, tvSource, "Wrong.11.PART1.mkv")
	status, initial, body = organizationPreview(t, tv, tvInput)
	if status != 200 {
		t.Fatal(status, body)
	}
	selection := assignmentFor(initial.Items[0], "tv", "200")
	season := 1
	part := "PARTC"
	selection.Season, selection.PartOverride, selection.EpisodeFormat, selection.EpisodeOffset = &season, &part, "Wrong.{ep}.PART1.mkv", -10
	tvInput.Assignments = []organizationMediaAssignment{selection}
	status, plan, body = organizationPreview(t, tv, tvInput)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].Part != "PARTC" || plan.Items[0].SeasonEpisode != "S01E01" {
		t.Fatal(status, body)
	}
	selection.EpisodeOffset = -7
	tvInput.Assignments = []organizationMediaAssignment{selection}
	status, plan, body = organizationPreview(t, tv, tvInput)
	if status != 200 || plan.Items[0].Status != "blocked" {
		t.Fatal("part override bypassed TV bound", status, body)
	}
}
