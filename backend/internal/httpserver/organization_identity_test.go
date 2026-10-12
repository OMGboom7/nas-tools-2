package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func assignmentFor(item organizationPlanItem, kind, id string) organizationMediaAssignment {
	return organizationMediaAssignment{Source: item.Source, Identity: item.Identity, Modified: item.Modified, Size: item.Size, Kind: kind, TMDBID: id}
}

func manualIdentityFixture(t *testing.T) (*subscriptionRunFixture, string, string, organizationPlanInput, *[]string) {
	t.Helper()
	f, source, target, input := organizationFixture(t)
	base := f.transport
	paths := []string{}
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" {
			paths = append(paths, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/3/search/") {
				return jsonResponse(r, `{"results":[]}`), nil
			}
			if r.URL.Path == "/3/movie/300" {
				return jsonResponse(r, `{"id":300,"title":"Corrected","release_date":"2025-01-01"}`), nil
			}
		}
		return base.RoundTrip(r)
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	return f, source, target, input, &paths
}

func TestOrganizationManualIdentityCorrectsUnrecognizedMediaAndCompanionsAndExecutesLiteralModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, paths := manualIdentityFixture(t)
			input.Mode = mode
			if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{name} ({year})/{title}-{videoFormat}"}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Unidentified.1999.1080p.mkv", "Unidentified.1999.1080p.en.srt"} {
				organizationFile(t, source, name)
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			status, original, body := organizationPreview(t, f, input)
			if status != 200 || original.Items[0].Status != "unrecognized" && original.Items[1].Status != "unrecognized" {
				t.Fatal(status, body)
			}
			for _, item := range original.Items {
				if item.Kind == "media" {
					input.Assignments = []organizationMediaAssignment{assignmentFor(item, "movie", "300")}
				}
			}
			*paths = nil
			status, reviewed, body := organizationPreview(t, f, input)
			if status != 200 || len(reviewed.Items) != 2 {
				t.Fatal(status, body)
			}
			for _, item := range reviewed.Items {
				if item.Status != "available" || !item.Assigned || item.TMDBID != "300" || item.Title != "Corrected" || item.Year != "2025" || !strings.HasPrefix(item.Target, "Corrected (2025)/Corrected-1080p.") {
					t.Fatal(item)
				}
			}
			for _, path := range *paths {
				if strings.HasPrefix(path, "/3/search/") {
					t.Fatal("manual identity searched guessed basename", path)
				}
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatal("preview wrote files", entries, err)
			}
			job := organizationCreateJob(t, f, input, reviewed)
			for _, item := range job.Items {
				if !item.Assigned || item.TMDBID != "300" {
					t.Fatal("lost manual provenance", item)
				}
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, item.Source)); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				} else {
					assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
				}
			}
			var count int
			var title string
			if err := f.db.QueryRow(`SELECT COUNT(*),TITLE FROM TRANSFER_HISTORY WHERE TMDBID=300`).Scan(&count, &title); err != nil || count != 1 || title != "Corrected" {
				t.Fatal(count, title, err)
			}
		})
	}
}

func TestOrganizationManualTVSelectionIsExplicitAndCannotBypassVerifiedSeasonBounds(t *testing.T) {
	f, source, target, input, _ := manualIdentityFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "Wrong.S09E99.1080p.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	selection := assignmentFor(initial.Items[0], "tv", "200")
	season, episode, last := 1, 2, 3
	selection.Season, selection.Episode, selection.EndEpisode = &season, &episode, &last
	input.Assignments = []organizationMediaAssignment{selection}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].SeasonEpisode != "S01E02-E03" || !strings.Contains(plan.Items[0].Target, "Show.S01E02-E03") {
		t.Fatal(status, body)
	}
	last = 4
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "blocked" {
		t.Fatal("accepted nonexistent episode", status, body)
	}
	input.Assignments[0].Season = nil
	status, _, body = organizationPreview(t, f, input)
	if status != 422 {
		t.Fatal("guessed missing season", status, body)
	}
	input.Assignments = []organizationMediaAssignment{assignmentFor(initial.Items[0], "movie", "300")}
	input.TargetID = subscriptionResourceKey("target:" + target + ":MOV")
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" || plan.Items[0].SeasonEpisode != "" || plan.Items[0].MediaType != "电影" {
		t.Fatal("movie retained filename TV metadata", status, body)
	}
}

func TestOrganizationManualIdentityRejectsInvalidOrStaleSelectionBeforeExternalLookup(t *testing.T) {
	f, source, _, input, paths := manualIdentityFixture(t)
	organizationFile(t, source, "Unidentified.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	base := assignmentFor(initial.Items[0], "movie", "300")
	for _, kind := range []string{"unknown-source", "escape", "identity", "size", "modified", "duplicate", "missing-id", "noncanonical-id", "unknown-kind", "movie-episode", "missing-season"} {
		t.Run(kind, func(t *testing.T) {
			selection := base
			expected := 422
			input.Assignments = nil
			switch kind {
			case "unknown-source":
				selection.Source = "other.mkv"
			case "escape":
				selection.Source = "../Unidentified.mkv"
			case "identity":
				selection.Identity = "1:1"
				expected = 409
			case "size":
				selection.Size++
				expected = 409
			case "modified":
				selection.Modified = "1"
				expected = 409
			case "missing-id":
				selection.TMDBID = ""
			case "noncanonical-id":
				selection.TMDBID = "0300"
			case "unknown-kind":
				selection.Kind = "any"
			case "movie-episode":
				season := 1
				selection.Season = &season
			case "missing-season":
				selection.Kind = "tv"
			}
			input.Assignments = []organizationMediaAssignment{selection}
			if kind == "duplicate" {
				input.Assignments = append(input.Assignments, selection)
			}
			*paths = nil
			status, _, body := organizationPreview(t, f, input)
			if status != expected || len(*paths) != 0 {
				t.Fatal(status, expected, body, *paths)
			}
		})
	}
}

func TestOrganizationManualCreationRequiresSameAssignmentsAndReviewedFingerprint(t *testing.T) {
	f, source, target, input, _ := manualIdentityFixture(t)
	organizationFile(t, source, "Unidentified.mkv")
	status, initial, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	input.Assignments = []organizationMediaAssignment{assignmentFor(initial.Items[0], "movie", "300")}
	status, reviewed, body := organizationPreview(t, f, input)
	if status != 200 || reviewed.Items[0].Status != "available" {
		t.Fatal(status, body)
	}
	for _, missing := range []bool{true, false} {
		changed := input
		if missing {
			changed.Assignments = nil
		} else {
			changed.Assignments = []organizationMediaAssignment{assignmentFor(initial.Items[0], "movie", "100")}
		}
		raw, _ := json.Marshal(struct {
			organizationPlanInput
			Fingerprint string `json:"fingerprint"`
		}{changed, reviewed.Fingerprint})
		response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
		if response.Code != 409 {
			t.Fatal("created unreviewed identity", response.Code, response.Body.String())
		}
	}
	job := organizationCreateJob(t, f, input, reviewed)
	if job.Items[0].TMDBID != "300" || !job.Items[0].Assigned {
		t.Fatal(job)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("create executed file operation", entries, err)
	}
}

func TestOrganizationManualIdentityKeepsFilesystemAndMetadataGates(t *testing.T) {
	for _, kind := range []string{"subtitle", "minimum", "bluray", "target-type", "details-id"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, input, paths := manualIdentityFixture(t)
			name := "Unidentified.mkv"
			if kind == "subtitle" {
				name = "Unidentified.srt"
			}
			if kind == "bluray" {
				name = "BDMV/STREAM/00001.m2ts"
			}
			organizationFile(t, source, name)
			status, initial, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			input.Assignments = []organizationMediaAssignment{assignmentFor(initial.Items[0], "movie", "300")}
			if kind == "minimum" {
				if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 150}); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "target-type" {
				input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
			}
			if kind == "details-id" {
				base := f.transport
				f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/3/movie/300" {
						return jsonResponse(r, `{"id":301,"title":"Wrong","release_date":"2025-01-01"}`), nil
					}
					return base.RoundTrip(r)
				})
				var err error
				f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
				if err != nil {
					t.Fatal(err)
				}
			}
			*paths = nil
			status, plan, body := organizationPreview(t, f, input)
			if kind == "subtitle" {
				if status != 422 {
					t.Fatal(status, body)
				}
			} else if status != 200 || plan.Items[0].Status != "blocked" {
				t.Fatal("manual identity bypassed gate", status, body)
			}
			if (kind == "minimum" || kind == "bluray" || kind == "subtitle") && len(*paths) != 0 {
				t.Fatal("lookup before safety gate", *paths)
			}
		})
	}
}

func TestOrganizationRequestsRejectUnreviewedFieldsAndTrailingJSON(t *testing.T) {
	f, _, _, _, _ := manualIdentityFixture(t)
	for _, body := range []string{`{"assignments":[{"target":"/outside"}]}`, `{} {"assignments":[]}`, `{} trailing`} {
		response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/plan", f.token, body)
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}
