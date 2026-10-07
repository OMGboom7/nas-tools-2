package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func organizationFixture(t *testing.T) (*subscriptionRunFixture, string, string, organizationPlanInput) {
	t.Helper()
	f := newSubscriptionRunFixture(t)
	source, target := t.TempDir(), t.TempDir()
	dirs, _ := json.Marshal([]map[string]string{{"save_path": "/remote-downloads", "container_path": source}})
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET DOWNLOAD_DIR=?`, string(dirs)); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": target, "media.tv_path": target, "media.movie_name_format": "{title} ({year})/{title} ({year})-{videoFormat}", "media.tv_name_format": "{title} ({year})/Season {season:0>2}/{title}.{season_episode}"}); err != nil {
		t.Fatal(err)
	}
	input := organizationPlanInput{SourceID: subscriptionResourceKey("source:" + source + ":"), TargetID: subscriptionResourceKey("target:" + target + ":MOV"), Path: ".", Mode: "copy"}
	return f, source, target, input
}

func organizationFile(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original-media"), 0600); err != nil {
		t.Fatal(err)
	}
}
func organizationPreview(t *testing.T, f *subscriptionRunFixture, input organizationPlanInput) (int, organizationPlan, string) {
	t.Helper()
	body, _ := json.Marshal(input)
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/plan", f.token, string(body))
	var payload struct{ Data organizationPlan }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return r.Code, payload.Data, r.Body.String()
}

func TestOrganizationPreviewUsesConfiguredRootsMetadataAndCompanionsWithoutWrites(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	for _, name := range []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt", "Movie.2026.1080p.mka", "Unrelated.srt", "Movie.2026.1080p.mkv.!qb"} {
		organizationFile(t, source, name)
	}
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if r.Code != 200 || strings.Contains(r.Body.String(), "private-downloader") || strings.Contains(r.Body.String(), "/remote-downloads") {
		t.Fatal(r.Code, r.Body.String())
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || !plan.PreviewOnly || len(plan.Items) != 4 || plan.Skipped != 1 || f.adds != 0 || f.queries != 0 {
		t.Fatal(status, plan, body)
	}
	for _, item := range plan.Items {
		info, err := os.Stat(filepath.Join(source, item.Source))
		if err != nil || item.Modified != strconv.FormatInt(info.ModTime().UnixNano(), 10) {
			t.Fatal("preview lost file timestamp precision", item, err)
		}
		if item.Source == "Unrelated.srt" {
			if item.Status != "unmatched" || item.Target != "" {
				t.Fatal(item)
			}
			continue
		}
		if item.Status != "available" || !strings.HasPrefix(item.Target, "Movie (2026)/Movie (2026)-1080p") || item.TMDBID != "100" {
			t.Fatal(item)
		}
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("preview wrote target", entries, err)
	}
	contents, err := os.ReadFile(filepath.Join(source, "Movie.2026.1080p.mkv"))
	if err != nil || string(contents) != "original-media" {
		t.Fatal("preview changed source", err)
	}
	var history bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='TRANSFER_HISTORY')`).Scan(&history); err != nil || history {
		t.Fatal("preview wrote history", history, err)
	}
}

func TestOrganizationPreviewTVChecksActualSeasonAndEpisodes(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	for _, name := range []string{"Show.S01E02.mkv", "Show.S01E99.mkv", "Show.S02E01.mkv", "Show.E01.mkv"} {
		organizationFile(t, source, name)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Source == "Show.S01E02.mkv" {
			if item.Status != "available" || item.Target != "Show (2026)/Season 01/Show.S01E02.mkv" {
				t.Fatal(item)
			}
		} else if item.Status == "available" {
			t.Fatal("unverified episode planned", item)
		}
	}
}

func TestOrganizationPreviewReportsExistingTargetsAndDuplicateSources(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.1080p.mkv")
	organizationFile(t, target, "Movie (2026)/Movie (2026)-1080p.mkv")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || len(plan.Items) != 1 || plan.Items[0].Status != "conflict" {
		t.Fatal(status, plan, body)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title}/{title}"}); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "Movie.2026.720p.mkv")
	organizationFile(t, source, "Movie.2026.1080p.srt")
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Kind == "media" && item.Status != "conflict" {
			t.Fatal(item)
		}
		if item.Kind == "subtitle" && item.Target != "" {
			t.Fatal("companion attached to conflicting movie", item)
		}
	}
}

func TestOrganizationPreviewRejectsUnsafeRootAndPathSelectors(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.mkv")
	if err := os.Symlink(t.TempDir(), filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../", "/", "escape/movie.mkv", "nested/../Movie.2026.mkv"} {
		input.Path = path
		status, _, _ := organizationPreview(t, f, input)
		if status != 422 {
			t.Fatal(path, status)
		}
	}
	input.Path = "."
	input.SourceID = "/etc"
	if status, _, _ := organizationPreview(t, f, input); status != 400 {
		t.Fatal(status)
	}
	input.SourceID = subscriptionResourceKey("source:" + source + ":")
	input.TargetID = "/outside"
	if status, _, _ := organizationPreview(t, f, input); status != 400 {
		t.Fatal(status)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": source}); err != nil {
		t.Fatal(err)
	}
	input.TargetID = subscriptionResourceKey("target:" + source + ":MOV")
	if status, _, _ := organizationPreview(t, f, input); status != 422 {
		t.Fatal("overlapping roots accepted", status, target)
	}
}

func TestOrganizationAdministratorAndConfiguredRootSelection(t *testing.T) {
	f, source, target, _ := organizationFixture(t)
	for _, path := range []string{"/api/v1/organization/roots", "/api/v1/organization/plan"} {
		method := "GET"
		if strings.HasSuffix(path, "plan") {
			method = "POST"
		}
		r := nativeJSONRequest(f.handler, method, path, "", `{}`)
		if r.Code != 401 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	if r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", viewer, ""); r.Code != 403 {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ENABLED=0; CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,ENABLED INTEGER); INSERT INTO CONFIG_SYNC_PATHS VALUES(1,?,?,1),(2,'/disabled','/disabled-target',0)`, source, target); err != nil {
		t.Fatal(err)
	}
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if r.Code != 200 || strings.Contains(r.Body.String(), "disabled") {
		t.Fatal(r.Code, r.Body.String())
	}
	var payload struct{ Data organizationRoots }
	if json.Unmarshal(r.Body.Bytes(), &payload) != nil || len(payload.Data.Sources) != 1 {
		t.Fatal(r.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, failure := (organizationAPI{service: f.runner.recognition.service, recognition: f.runner.recognition}).plan(ctx, organizationPlanInput{})
	if failure == nil {
		t.Fatal("cancelled preview succeeded")
	}
}

func TestOrganizationUnsupportedTemplatesAndBluRayStayBlocked(t *testing.T) {
	f, source, _, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.mkv")
	organizationFile(t, source, "BDMV/STREAM/Movie.2026.m2ts")
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title}/{unsupported}"}); err != nil {
		t.Fatal(err)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || len(plan.Items) != 2 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status != "blocked" || item.Target != "" {
			t.Fatal(item)
		}
	}
}

func TestOrganizationSyncTargetDoesNotRequireDefaultLibraryRoot(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.1080p.mkv")
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,ENABLED INTEGER); INSERT INTO CONFIG_SYNC_PATHS VALUES(1,?,?,1)`, source, target); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": "", "media.tv_path": ""}); err != nil {
		t.Fatal(err)
	}
	input.TargetID = subscriptionResourceKey("target:" + target + ":")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || len(plan.Items) != 1 || plan.Items[0].Status != "available" {
		t.Fatal(status, plan, body)
	}
}

func TestOrganizationPreviewCapacityFailsBeforeMetadataAndCancellationJoins(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		f, source, _, input := organizationFixture(t)
		for i := 0; i < 101; i++ {
			organizationFile(t, source, fmt.Sprintf("Movie.%d.mkv", i))
		}
		status, plan, _ := organizationPreview(t, f, input)
		if status != 422 || len(plan.Items) != 0 {
			t.Fatal(status, plan)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f, source, _, input := organizationFixture(t)
		organizationFile(t, source, "Movie.2026.mkv")
		started := make(chan struct{})
		base := f.transport
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasPrefix(r.URL.Path, "/3/search/") {
				close(started)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return base.RoundTrip(r)
		})
		var err error
		f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		body, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", "/api/v1/organization/plan", strings.NewReader(string(body))).WithContext(ctx)
		r.Header.Set("Authorization", f.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("metadata lookup did not start")
		}
		cancel()
		waitSubscriptionWorker(t, done)
		if w.Code != 504 || strings.Contains(w.Body.String(), "private-downloader") {
			t.Fatal(w.Code, w.Body.String())
		}
	})
}
