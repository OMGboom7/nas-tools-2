package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func legacyCustomRequest(t *testing.T, f *subscriptionRunFixture, input map[string]any) (*httptest.ResponseRecorder, string) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/renameudf", f.token, string(raw))
	var result struct{ Data struct{ JobID string } }
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w, result.Data.JobID
}

func TestLegacyCustomRenameExecutesLiteralModesAndPersistsChoice(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, _, _ := organizationLinkFixture(t, mode)
			w, id := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": target, "syncmod": mode, "type": "MOV", "tmdb": 100, "min_filesize": 0, "confirmSourceRemoval": mode == "move"})
			if w.Code != 200 || id == "" || !strings.Contains(w.Body.String(), `"success":true`) {
				t.Fatal(w.Code, w.Body.String())
			}
			job := organizationReadMoveJob(t, f, id)
			if job.State != "completed" || job.Mode != mode || job.LegacyOrigin == nil || job.LegacyOrigin.Action != "renameudf" || job.LegacyOrigin.Actor == "" || job.LegacySelection == nil || job.LegacySelection.TMDBID != "100" || len(job.Items) != 2 {
				t.Fatal(job)
			}
			for _, item := range job.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			path := filepath.Join(source, "Movie.2026.1080p.mkv")
			if mode == "move" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				assertOrganizationMoveBytes(t, path, "original-media")
			}
		})
	}
}

func TestLegacyCustomTVFilteringOffsetAndCompanions(t *testing.T) {
	f, source, target, _ := organizationFixture(t)
	for _, name := range []string{"Show.11.mkv", "Show.11.en.srt", "Show.12.mkv", "Show.12.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	w, id := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": target, "syncmod": "复制", "type": "TV", "tmdb": "200", "season": "1", "episode_format": "Show.{ep}.mkv", "episode_details": "11,11", "episode_offset": "-10"})
	if w.Code != 200 || id == "" || !strings.Contains(w.Body.String(), `"filtered":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, id)
	if len(job.Items) != 2 || job.LegacySelection.EpisodeOffset != -10 {
		t.Fatal(job)
	}
	for _, item := range job.Items {
		if !strings.Contains(item.Target, "S01E01") {
			t.Fatal(item)
		}
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Show.12.mkv"), "original-media")
}

func TestLegacyCustomExplicitEpisodeDetailsAndDefaultSeason(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	w, id := legacyCustomRequest(t, f, map[string]any{"inpath": filepath.Join(source, "Movie.2026.1080p.mkv"), "outpath": target, "syncmod": "copy", "type": "TV", "tmdb": 200, "episode_details": "2-3"})
	if w.Code != 200 || id == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, id)
	if len(job.Items) != 1 || !strings.Contains(job.Items[0].Target, "S01E02-E03") {
		t.Fatal(job)
	}
}

func TestLegacyCustomRejectsInvalidOrUnsupportedRequestsBeforeJobs(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	for _, extra := range []map[string]any{
		{"syncmod": "move"}, {"syncmod": "Rclone移动"}, {"type": "UNKNOWN"}, {"tmdb": "900719925474099312345"},
		{"type": "TV", "episode_details": "3,2"}, {"type": "TV", "episode_details": "1,3"},
		{"type": "MOV", "season": 1}, {"type": "TV", "season": 100}, {"episode_offset": 1.5},
		{"min_filesize": -1}, {"actor": "forged"}, {"title": "forged"}, {"confirmSourceRemoval": "yes"},
		{"inpath": t.TempDir()}, {"outpath": t.TempDir()}, {"outpath": filepath.Join(target, "new-subdirectory")},
	} {
		input := map[string]any{"inpath": source, "outpath": target, "syncmod": "copy", "type": "MOV", "tmdb": "100"}
		for k, v := range extra {
			input[k] = v
		}
		w, id := legacyCustomRequest(t, f, input)
		if w.Code < 400 || id != "" {
			t.Fatal(input, w.Code, w.Body.String())
		}
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}

func TestLegacyCustomFormAndConflictDoNotOverwriteOrRetry(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	form := url.Values{"inpath": {source}, "outpath": {target}, "syncmod": {"复制"}, "type": {"MOV"}, "tmdb": {"100"}}
	w := performFormRequest(f.handler, "/api/v1/organization/unknown/renameudf", f.token, form)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.handler, "/api/v1/organization/unknown/renameudf", f.token, form)
	if w.Code != 422 {
		t.Fatal(w.Code, w.Body.String())
	}
	form.Add("tmdb", "100")
	w = performFormRequest(f.handler, "/api/v1/organization/unknown/renameudf", f.token, form)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestLegacyCustomRequestMinimumDoesNotChangeConfigAndSymlinksAreRejected(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 100}); err != nil {
		t.Fatal(err)
	}
	w, id := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": target, "syncmod": "copy", "min_filesize": 0})
	if w.Code != 200 || id == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	snapshot, err := config.NewStore(f.path).Snapshot()
	if err != nil || text(objectValue(snapshot["media"])["min_filesize"]) != "100" {
		t.Fatal(snapshot, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	w, id = legacyCustomRequest(t, f, map[string]any{"inpath": filepath.Join(source, "escape"), "outpath": target, "syncmod": "copy", "min_filesize": 0})
	if w.Code < 400 || id != "" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLegacyCustomRevalidatesPlanAndUncertainExecutionHasJobID(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	// A quiet-source violation is detected by execution after durable reservation.
	path := filepath.Join(source, "Movie.2026.1080p.mkv")
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	w, id := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": target, "syncmod": "copy", "type": "MOV", "tmdb": "100"})
	if w.Code < 400 || id == "" || strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, id)
	if job.State == "completed" {
		t.Fatal(job)
	}
	assertOrganizationMoveBytes(t, path, "original-media")
}

func TestLegacyCustomPlanChangeAndAuthorization(t *testing.T) {
	f, source, target, _, _ := organizationLinkFixture(t, "copy")
	base := f.transport
	calls := 0
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" && r.URL.Path == "/3/movie/100" {
			calls++
			if calls == 2 {
				return jsonResponse(r, `{"id":100,"title":"Changed","release_date":"2026-01-01","genres":[]}`), nil
			}
		}
		return base.RoundTrip(r)
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w, id := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": target, "syncmod": "copy", "tmdb": "100", "type": "MOV"})
	if w.Code != 409 || id != "" {
		t.Fatal(calls, w.Code, w.Body.String())
	}
	for _, token := range []string{"", "forged"} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/renameudf", token, `{}`)
		if w.Code != 401 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
