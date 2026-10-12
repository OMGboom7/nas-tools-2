package httpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestOrganizationPartsKeepDistinctTargetsAndCompanionsInAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input := organizationFixture(t)
			input.Mode = mode
			if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title} ({year})/{title}-{part}"}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Movie.2026.CD1.mkv", "Movie.2026.CD2.mkv", "Movie.2026.CD1.en.srt", "Movie.2026.CD2.en.srt"} {
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
			for _, item := range plan.Items {
				if item.Status != "available" || item.Part != "CD1" && item.Part != "CD2" || !strings.Contains(item.Target, "Movie-"+item.Part) || item.MediaType != "电影" {
					t.Fatal(item)
				}
			}
			job := organizationCreateJob(t, f, input, plan)
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				if item.Part == "" {
					t.Fatal("lost part", item)
				}
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 2 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestOrganizationPartsDoNotOverrideTemplatesOrGuessAmbiguousMarkers(t *testing.T) {
	f, source, _, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.CD1.mkv")
	organizationFile(t, source, "Movie.2026.CD2.mkv")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status != "conflict" {
			t.Fatal("silently changed template without part", item)
		}
	}
	organizationFile(t, source, "Other.2026.CD1.CD2.mkv")
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if strings.HasPrefix(item.Source, "Other") && item.Status != "blocked" {
			t.Fatal("guessed ambiguous part", item)
		}
	}
}

func TestOrganizationTVPartKeepsVerifiedEpisodeAndManualIdentityPart(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	if err := config.NewStore(f.path).Update(map[string]any{"media.tv_name_format": "{title}/Season {season}/{title}.{season_episode}-{part}"}); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "Show.S01E02.PART1.mkv")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].Part != "PART1" || plan.Items[0].SeasonEpisode != "S01E02" || !strings.HasSuffix(plan.Items[0].Target, "S01E02-PART1.mkv") {
		t.Fatal(status, body)
	}
	selection := assignmentFor(plan.Items[0], "tv", "200")
	season, episode := 1, 2
	selection.Season = &season
	selection.Episode = &episode
	input.Assignments = []organizationMediaAssignment{selection}
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 || !plan.Items[0].Assigned || plan.Items[0].Part != "PART1" {
		t.Fatal(status, body)
	}
}
