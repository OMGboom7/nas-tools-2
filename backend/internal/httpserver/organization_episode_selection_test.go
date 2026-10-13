package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func batchEpisodeRule() *organization.EpisodeSelection {
	season, first, last := 1, 11, 11
	return &organization.EpisodeSelection{TMDBID: "200", Season: &season, EpisodeFormat: "Wrong.{ep}.mkv", StartEpisode: &first, EndEpisode: &last, EpisodeOffset: -10}
}

func TestOrganizationEpisodeSelectionTransfersOnlyReviewedRangeAndCompanionsInAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, multi := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{true: "range", false: "single"}[multi], func(t *testing.T) {
				f, source, target, input, paths := manualIdentityFixture(t)
				input.Mode, input.TargetID, input.EpisodeSelection = mode, subscriptionResourceKey("target:"+target+":TV"), batchEpisodeRule()
				selected := "Wrong.11"
				wanted := "S01E01"
				if multi {
					selected, wanted = "Wrong.EP11-EP13", "S01E01-E03"
				}
				for _, name := range []string{selected + ".mkv", selected + ".en.srt", "Wrong.10.mkv", "Wrong.10.en.srt", "Wrong.14.mkv", "Other.mkv", "Other.en.srt"} {
					organizationFile(t, source, name)
					old := time.Now().Add(-time.Hour)
					if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
						t.Fatal(err)
					}
				}
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 || len(plan.Items) != 7 {
					t.Fatal(status, body)
				}
				available, filtered := 0, 0
				for _, item := range plan.Items {
					if item.Status == "available" {
						available++
						if item.SeasonEpisode != wanted || !item.Assigned || item.EpisodeOffset != -10 {
							t.Fatal(item)
						}
					} else if item.Status == "filtered" {
						filtered++
					} else {
						t.Fatal("excluded file not explicitly filtered", item)
					}
				}
				if available != 2 || filtered != 5 {
					t.Fatal(available, filtered)
				}
				for _, path := range *paths {
					if path != "/3/tv/200" {
						t.Fatal("batch guessed filename identity", path)
					}
				}
				if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
					t.Fatal("preview wrote target", entries, err)
				}
				job := organizationCreateJob(t, f, input, plan)
				if len(job.Items) != 2 || job.EpisodeSelection == nil || *job.EpisodeSelection.StartEpisode != 11 {
					t.Fatal(job)
				}
				status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
				if status != 200 || done.State != "completed" {
					t.Fatal(status, body)
				}
				for _, item := range done.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				for _, name := range []string{"Wrong.10.mkv", "Wrong.10.en.srt", "Wrong.14.mkv", "Other.mkv", "Other.en.srt"} {
					assertOrganizationMoveBytes(t, filepath.Join(source, name), "original-media")
				}
				var count int
				var episode string
				if err := f.db.QueryRow(`SELECT COUNT(*),SEASON_EPISODE FROM TRANSFER_HISTORY`).Scan(&count, &episode); err != nil || count != 1 || episode != wanted {
					t.Fatal(count, episode, err)
				}
			})
		}
	}
}

func TestOrganizationEpisodeSelectionRejectsInvalidRulesBeforeLookup(t *testing.T) {
	f, source, target, input, paths := manualIdentityFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "Wrong.11.mkv")
	for _, kind := range []string{"id", "season", "template", "missing-filter", "reversed", "offset", "part", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			rule := batchEpisodeRule()
			input.Assignments = nil
			switch kind {
			case "id":
				rule.TMDBID = "0200"
			case "season":
				rule.Season = nil
			case "template":
				rule.EpisodeFormat = "Wrong.{ep:d}.mkv"
			case "missing-filter":
				rule.EndEpisode = nil
			case "reversed":
				n := 10
				rule.EndEpisode = &n
			case "offset":
				rule.EpisodeOffset = 100000
			case "part":
				p := "../CD1"
				rule.PartOverride = &p
			case "mixed":
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 {
					t.Fatal(status, body)
				}
				input.Assignments = []organizationMediaAssignment{assignmentFor(plan.Items[0], "movie", "300")}
			}
			input.EpisodeSelection = rule
			*paths = nil
			status, _, body := organizationPreview(t, f, input)
			if status != 422 || len(*paths) != 0 {
				t.Fatal(status, body, *paths)
			}
			input.EpisodeSelection = nil
		})
	}
	input.Assignments = nil
	input.EpisodeSelection = batchEpisodeRule()
	input.EpisodeSelection.EpisodeOffset = -7
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "blocked" {
		t.Fatal("bypassed actual season limit", status, body)
	}
}

func batchCreateResponse(f *subscriptionRunFixture, input organizationPlanInput, fingerprint string) (int, string) {
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, fingerprint})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	return w.Code, w.Body.String()
}

func TestOrganizationEpisodeSelectionFingerprintIncludesFilterAndExcludedSourceSnapshot(t *testing.T) {
	f, source, target, input, _ := manualIdentityFixture(t)
	input.TargetID, input.EpisodeSelection = subscriptionResourceKey("target:"+target+":TV"), batchEpisodeRule()
	organizationFile(t, source, "Wrong.11.mkv")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" {
		t.Fatal(status, body)
	}
	changed := *input.EpisodeSelection
	n := 12
	changed.EndEpisode = &n // same selected files, different reviewed filter.
	input.EpisodeSelection = &changed
	status, body = batchCreateResponse(f, input, plan.Fingerprint)
	if status != 409 {
		t.Fatal("accepted changed filter", status, body)
	}
	input.EpisodeSelection = batchEpisodeRule()
	organizationFile(t, source, "Wrong.14.mkv") // excluded, but visible review changed.
	status, body = batchCreateResponse(f, input, plan.Fingerprint)
	if status != 409 {
		t.Fatal("accepted stale source scope", status, body)
	}
}

func TestOrganizationEpisodeSelectionCannotSettlePartiallyFilteredUnknownScope(t *testing.T) {
	f, source, target, input := unknownFixture(t, "copy")
	input.TargetID, input.EpisodeSelection = subscriptionResourceKey("target:"+target+":TV"), batchEpisodeRule()
	organizationFile(t, source, "Wrong.11.mkv")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, body = batchCreateResponse(f, input, plan.Fingerprint)
	if status != 422 || unknownState(t, f, 7) != "N" {
		t.Fatal("partial scope closed unknown record", status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
