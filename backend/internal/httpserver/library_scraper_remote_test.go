package httpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

type remoteLibraryHTTP struct {
	base                           *libraryHTTPFixture
	driver, root, logical, account string
	cloud                          *s3test.Server
	cfg                            config.Config
}

func newRemoteLibraryHTTP(t *testing.T, driver string, tv bool) *remoteLibraryHTTP {
	t.Helper()
	x := &remoteLibraryHTTP{base: newLibraryHTTPFixture(t, tv), driver: driver, logical: "/bucket/library"}
	if driver == "minio" {
		x.cloud, x.cfg = minioHTTP(t, x.base.f)
		x.account = x.cfg.MinioConfigFile
	} else {
		binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
		if binary == "" {
			t.Skip("explicit isolated Rclone required")
		}
		alias := t.TempDir()
		x.root = filepath.Join(alias, "bucket/library")
		if err := os.MkdirAll(x.root, 0700); err != nil {
			t.Fatal(err)
		}
		x.account = filepath.Join(t.TempDir(), "rclone.conf")
		if err := os.WriteFile(x.account, []byte("[NASTOOL]\ntype=alias\nremote="+alias+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := config.NewStore(x.base.f.path).Update(map[string]any{"media.movie_path": x.logical, "media.tv_path": x.logical, "media.anime_path": x.logical}); err != nil {
			t.Fatal(err)
		}
		x.cfg = config.Config{ApplicationConfigPath: x.base.f.path, DisableLegacy: true, RcloneBinary: binary, RcloneConfigFile: x.account}
		x.restart(t)
	}
	x.base.root = x.logical
	kind := "MOV"
	if tv {
		kind = "TV"
	}
	x.base.input = libraryPlanInput{Driver: driver, RootID: subscriptionResourceKey("target:" + x.logical + ":" + kind), Path: ".", Mode: "no_force"}
	return x
}
func (x *remoteLibraryHTTP) restart(t *testing.T) {
	t.Helper()
	var err error
	x.base.f.handler, err = buildHandler(x.cfg, x.base.f.transport, &x.base.f.runner)
	if err != nil {
		t.Fatal(err)
	}
	if x.cfg.RcloneConfigFile != "" {
		x.base.f.runner.organizer.rclone.Environment = []string{}
	}
}
func (x *remoteLibraryHTTP) put(t *testing.T, name string, data []byte) {
	t.Helper()
	if x.cloud != nil {
		x.cloud.Put("bucket/library/"+name, data)
		return
	}
	file := filepath.Join(x.root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (x *remoteLibraryHTTP) read(t *testing.T, name string) ([]byte, bool) {
	t.Helper()
	if x.cloud != nil {
		return x.cloud.Get("bucket/library/" + name)
	}
	data, err := os.ReadFile(filepath.Join(x.root, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return data, err == nil
}
func (x *remoteLibraryHTTP) create(t *testing.T, p libraryPreview) organization.LibraryJob {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"driver": p.Input.Driver, "rootId": p.Input.RootID, "path": p.Input.Path, "mode": p.Input.Mode, "excludes": p.Input.Excludes, "requestId": p.Input.RequestID, "allowDiscExpansion": p.Input.AllowDiscExpansion, "fingerprint": p.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": true, "confirmOverwrite": true, "confirmRemoteExclusive": true, "confirmRemoteRecoveryCopy": true})
	r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/jobs", x.base.f.token, string(raw))
	var result struct{ Data organization.LibraryJob }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return result.Data
}
func (x *remoteLibraryHTTP) tick(t *testing.T) {
	t.Helper()
	if err := x.base.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func (x *remoteLibraryHTTP) outputs(t *testing.T, id string) []organization.LibraryOutput {
	t.Helper()
	out, err := x.base.f.runner.organizer.jobs.LibraryOutputs(t.Context(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (x *remoteLibraryHTTP) reviewOutput(t *testing.T, id string) map[string]any {
	t.Helper()
	r := nativeJSONRequest(x.base.f.handler, "GET", libraryEndpoint+"/outputs/"+id, x.base.f.token, "")
	var result struct{ Data map[string]any }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return result.Data
}
func (x *remoteLibraryHTTP) recover(t *testing.T, id, action string, review map[string]any) organization.LibraryOutput {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"fingerprint": review["fingerprint"], "confirm": true, "confirmOldExecutorsStopped": true, "confirmRemoteExclusive": true, "confirmRemoteRecoveryCopy": true})
	r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/outputs/"+id+"/"+action, x.base.f.token, string(raw))
	var result struct{ Data organization.LibraryOutput }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return result.Data
}
func (x *remoteLibraryHTTP) assertNoHistory(t *testing.T) {
	t.Helper()
	var exists bool
	if err := x.base.f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='TRANSFER_HISTORY')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return
	}
	var count int
	if err := x.base.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 0 {
		t.Fatal("existing library generated intake history", count, err)
	}
}

func TestRemoteLibraryManualThreeModesAndRestoreAcrossProvidersNeverUseLocalNamesakes(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		for _, mode := range []string{"no_force", "force_nfo", "force_all"} {
			t.Run(driver+"/"+mode, func(t *testing.T) {
				x := newRemoteLibraryHTTP(t, driver, false)
				x.base.input.Mode = mode
				x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
				oldNFO := []byte(`<movie><uniqueid type="tmdb" default="true">101</uniqueid></movie>`)
				x.put(t, "Movie/Movie.2026.nfo", oldNFO)
				x.put(t, "Movie/poster.png", []byte("existing image not validated"))
				p := x.base.preview(t)
				if p.Remote == nil || p.Remote.Driver != driver || p.Total != 1 || p.Blocked != 0 {
					t.Fatal(p)
				}
				bad, _ := json.Marshal(map[string]any{"driver": driver, "rootId": p.Input.RootID, "path": p.Input.Path, "mode": mode, "excludes": p.Input.Excludes, "requestId": p.Input.RequestID, "fingerprint": p.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": true, "confirmOverwrite": true})
				if r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/jobs", x.base.f.token, string(bad)); r.Code != 400 {
					t.Fatal("implicit remote consent", r.Code)
				}
				j := x.create(t, p)
				if j.Remote == nil || j.Remote.Driver != driver {
					t.Fatal(j)
				}
				x.tick(t)
				job := x.base.job(t, j.ID)
				if job.State != "completed" {
					t.Fatal(job)
				}
				out := x.outputs(t, j.ID)
				if len(out) != 2 {
					t.Fatal(out)
				}
				for _, output := range out {
					expect := "preserved"
					if mode == "force_all" || mode == "force_nfo" && strings.HasSuffix(output.Target, ".nfo") {
						expect = "replaced"
					}
					if output.State != expect {
						t.Fatal(output, expect)
					}
					if output.State == "replaced" {
						backup, ok := x.read(t, path.Join(output.Proof.Stage, "old"))
						if !ok || len(backup) == 0 {
							t.Fatal("original backup missing")
						}
						view := x.reviewOutput(t, output.ID)
						if view["canRestore"] != true {
							t.Fatal(view)
						}
						bad, _ := json.Marshal(map[string]any{"fingerprint": view["fingerprint"], "confirm": true, "confirmOldExecutorsStopped": true})
						if r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/outputs/"+output.ID+"/restore", x.base.f.token, string(bad)); r.Code != 400 {
							t.Fatal("restore reused old consent", r.Code)
						}
						if done := x.recover(t, output.ID, "restore", view); done.State != "restored" {
							t.Fatal(done)
						}
						actual, _ := x.read(t, output.Target)
						if !bytes.Equal(actual, backup) {
							t.Fatal("restore did not return original bytes")
						}
					}
				}
				media, _ := x.read(t, "Movie/Movie.2026.mkv")
				if string(media) != "original-media" {
					t.Fatal("media changed")
				}
				x.assertNoHistory(t)
				lookups, images := x.base.lookups.Load(), x.base.images.Load()
				x.restart(t)
				x.tick(t)
				if x.base.lookups.Load() != lookups || x.base.images.Load() != images {
					t.Fatal("restart repeated existing-library work")
				}
				status := nativeJSONRequest(x.base.f.handler, "GET", libraryEndpoint, x.base.f.token, "")
				if status.Code != 200 || strings.Contains(status.Body.String(), x.account) || strings.Contains(status.Body.String(), s3test.SecretKey) {
					t.Fatal("library receipt leaked account", status.Code)
				}
			})
		}
	}
}

func TestRemoteLibraryUncertainPublicationAndLostJournalAreReadonlyReconciledWithoutReplay(t *testing.T) {
	for _, mode := range []string{"remote-response", "journal-response"} {
		t.Run(mode, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, "minio", false)
			x.base.input.Mode = "force_all"
			x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
			x.put(t, "Movie/Movie.2026.nfo", []byte("old metadata"))
			p := x.base.preview(t)
			j := x.create(t, p)
			if mode == "remote-response" {
				x.cloud.FailNextWrite("bucket/library/Movie/Movie.2026.nfo", false, 500)
			} else {
				if _, err := x.base.f.db.Exec(`CREATE TRIGGER lose_remote_library_result BEFORE UPDATE OF STATE ON GO_LIBRARY_SCRAPE_OUTPUTS WHEN NEW.STATE='replaced' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
					t.Fatal(err)
				}
			}
			x.tick(t)
			job := x.base.job(t, j.ID)
			if job.State != "needs_review" {
				t.Fatal(job)
			}
			outputs := x.outputs(t, j.ID)
			if len(outputs) != 1 || outputs[0].State != "uncertain" && outputs[0].State != "writing" {
				t.Fatal(outputs)
			}
			view := x.reviewOutput(t, outputs[0].ID)
			if view["evidence"] != "replaced" || view["canReconcile"] != true {
				t.Fatal(view)
			}
			if mode == "journal-response" {
				if _, err := x.base.f.db.Exec(`DROP TRIGGER lose_remote_library_result`); err != nil {
					t.Fatal(err)
				}
			}
			before := len(x.cloud.Requests())
			done := x.recover(t, outputs[0].ID, "reconcile", view)
			if done.State != "replaced" {
				t.Fatal(done)
			}
			for _, r := range x.cloud.Requests()[before:] {
				if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
					t.Fatal("readonly reconcile replayed metadata")
				}
			}
			view = x.reviewOutput(t, outputs[0].ID)
			x.cloud.FailNextWrite("bucket/library/Movie/Movie.2026.nfo", false, 500)
			raw, _ := json.Marshal(map[string]any{"fingerprint": view["fingerprint"], "confirm": true, "confirmOldExecutorsStopped": true, "confirmRemoteExclusive": true, "confirmRemoteRecoveryCopy": true})
			if r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/outputs/"+outputs[0].ID+"/restore", x.base.f.token, string(raw)); r.Code < 400 {
				t.Fatal("lost restore reported success")
			}
			view = x.reviewOutput(t, outputs[0].ID)
			if view["canRestore"] != true {
				t.Fatal(view)
			}
			before = len(x.cloud.Requests())
			if done := x.recover(t, outputs[0].ID, "restore", view); done.State != "restored" {
				t.Fatal(done)
			}
			for _, r := range x.cloud.Requests()[before:] {
				if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
					t.Fatal("positive restore receipt rewrote original")
				}
			}
			x.restart(t)
			x.tick(t)
			x.assertNoHistory(t)
		})
	}
}

func TestRemoteLibraryStoppingAndRevokingConfigCancelTheCurrentOperationWithoutRetry(t *testing.T) {
	for _, mode := range []string{"stop", "config", "account"} {
		t.Run(mode, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, "minio", false)
			x.base.input.Mode = "force_all"
			x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
			x.put(t, "Movie/Movie.2026.nfo", []byte("old metadata"))
			j := x.create(t, x.base.preview(t))
			started := make(chan struct{})
			var calls atomic.Int32
			base := x.base.f.runner.scraper.transport
			x.base.f.runner.scraper.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
					if calls.Add(1) == 1 {
						close(started)
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return base.RoundTrip(r)
			})
			done := make(chan error, 1)
			go func() { done <- x.base.f.runner.library.tick(t.Context()) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("remote metadata lookup not started")
			}
			switch mode {
			case "stop":
				r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/jobs/"+j.ID+"/cancel", x.base.f.token, `{"confirm":true}`)
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
			case "config":
				if err := x.base.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{}`); err != nil {
					t.Fatal(err)
				}
			case "account":
				raw, _ := os.ReadFile(x.account)
				if err := os.WriteFile(x.account, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("operation ignored stop/revocation")
			}
			data, _ := x.read(t, "Movie/Movie.2026.nfo")
			if string(data) != "old metadata" {
				t.Fatal("revoked metadata was overwritten")
			}
			x.base.f.runner.scraper.transport = base
			x.restart(t)
			x.tick(t)
			if calls.Load() != 1 {
				t.Fatal("interrupted item replayed")
			}
		})
	}
}

func TestRemoteLibraryUnknownAccountAndLateInventoryNeverFallBackToLocalFilesystem(t *testing.T) {
	for _, driver := range []string{"rclone", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, driver, false)
			x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
			p := x.base.preview(t)
			x.put(t, "Movie/Another.2026.mkv", []byte("late-media"))
			raw, _ := json.Marshal(map[string]any{"driver": driver, "rootId": p.Input.RootID, "path": p.Input.Path, "mode": p.Input.Mode, "excludes": p.Input.Excludes, "requestId": p.Input.RequestID, "fingerprint": p.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": true, "confirmOverwrite": true, "confirmRemoteExclusive": true, "confirmRemoteRecoveryCopy": true})
			if r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/jobs", x.base.f.token, string(raw)); r.Code != 409 {
				t.Fatal("late file silently joined review", r.Code, r.Body.String())
			}
			if err := os.WriteFile(x.account, []byte("not a usable account"), 0600); err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(x.base.input)
			if r := nativeJSONRequest(x.base.f.handler, "POST", libraryEndpoint+"/preview", x.base.f.token, string(raw)); r.Code < 400 {
				t.Fatal("missing account used local path")
			}
			x.assertNoHistory(t)
		})
	}
}

func TestRemoteLibraryRealSeekableFFmpegProducesExistingLibraryThumbnailWithoutIntake(t *testing.T) {
	binaryPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, driver, true)
			file := filepath.Join(t.TempDir(), "synthetic.mp4")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "183", "-c:v", "mpeg4", "-threads", "1", file)
			cmd.Env = []string{"LANG=C", "LC_ALL=C"}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(b))
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			moov, mdat := bytes.Index(data, []byte("moov"))-4, bytes.Index(data, []byte("mdat"))-4
			if moov <= mdat || mdat < 0 {
				t.Fatal("not an end-moov video")
			}
			padding := make([]byte, 8<<20)
			binary.BigEndian.PutUint32(padding, uint32(len(padding)))
			copy(padding[4:], "free")
			data = append(append(append([]byte{}, data[:moov]...), padding...), data[moov:]...)
			relative := "Show/Season 01/Show.S01E02.mp4"
			x.put(t, relative, data)
			if err := x.base.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"tv":{"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
				t.Fatal(err)
			}
			j := x.create(t, x.base.preview(t))
			x.tick(t)
			if job := x.base.job(t, j.ID); job.State != "completed" {
				t.Fatal(job)
			}
			thumb, exists := x.read(t, "Show/Season 01/Show.S01E02-thumb.jpg")
			if !exists || scraper.ValidateImage(thumb, ".jpg") != nil {
				t.Fatal("real existing-library thumbnail missing")
			}
			media, _ := x.read(t, relative)
			if !bytes.Equal(media, data) {
				t.Fatal("decoder changed existing media")
			}
			x.assertNoHistory(t)
			lookups := x.base.lookups.Load()
			x.restart(t)
			x.tick(t)
			after, _ := x.read(t, "Show/Season 01/Show.S01E02-thumb.jpg")
			if !bytes.Equal(after, thumb) || x.base.lookups.Load() != lookups {
				t.Fatal("restart replayed frame")
			}
		})
	}
}

func TestRemoteLibraryDiscAndReviewedLegacyTriggersUseOneWholeReadonlyMedia(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, driver, false)
			files := map[string]string{"Movie.2026/BDMV/index.bdmv": "index", "Movie.2026/BDMV/STREAM/00000.m2ts": "stream", "Movie.2026/CERTIFICATE/id.bdmv": "certificate"}
			for name, data := range files {
				x.put(t, name, []byte(data))
			}
			x.base.input.Path = "Movie.2026/BDMV/STREAM/00000.m2ts"
			x.base.input.AllowDiscExpansion = true
			p := x.base.preview(t)
			if p.Total != 1 || len(p.Sample) != 1 || p.Sample[0].Disc == nil || !p.Sample[0].Disc.Expanded {
				t.Fatal(p)
			}
			j := x.create(t, p)
			x.tick(t)
			if job := x.base.job(t, j.ID); job.State != "completed" {
				t.Fatal(job)
			}
			for name, want := range files {
				data, _ := x.read(t, name)
				if string(data) != want {
					t.Fatal("disc payload changed", name)
				}
			}
			for _, out := range x.outputs(t, j.ID) {
				if strings.Contains(out.Target, "/BDMV/") || strings.Contains(out.Target, "/CERTIFICATE/") {
					t.Fatal("metadata published inside disc internals", out.Target)
				}
			}
			x.assertNoHistory(t)
		})
	}
	for _, driver := range []string{"minio", "rclone"} {
		for _, event := range []bool{false, true} {
			t.Run(driver+map[bool]string{false: "/legacy", true: "/event"}[event], func(t *testing.T) {
				x := newRemoteLibraryHTTP(t, driver, false)
				x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
				if event {
					libraryPluginInstall(t, x.base)
				}
				endpoint := "/do"
				if event {
					endpoint = libraryEndpoint + "/event"
				}
				input := libraryTriggerInput{Driver: driver, Path: x.logical + "/Movie", Force: true, Review: true}
				r := nativeJSONRequest(x.base.f.handler, "POST", endpoint, x.base.f.token, libraryTriggerBody(t, event, input))
				var p struct{ Data libraryPreview }
				if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil || p.Data.Total != 1 || p.Data.Remote == nil {
					t.Fatal(r.Code, r.Body.String())
				}
				input.Review = false
				input.RequestID = p.Data.Input.RequestID
				input.Fingerprint = p.Data.Fingerprint
				input.Confirm = true
				input.ConfirmOldExecutorsStopped = true
				input.ConfirmOverwrite = true
				if r := nativeJSONRequest(x.base.f.handler, "POST", endpoint, x.base.f.token, libraryTriggerBody(t, event, input)); r.Code != 400 {
					t.Fatal("event inherited remote grant", r.Code)
				}
				input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy = true, true
				r = nativeJSONRequest(x.base.f.handler, "POST", endpoint, x.base.f.token, libraryTriggerBody(t, event, input))
				var job struct{ Data organization.LibraryJob }
				if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &job) != nil || job.Data.Remote == nil {
					t.Fatal(r.Code, r.Body.String())
				}
				x.tick(t)
				if current := x.base.job(t, job.Data.ID); current.State != "completed" {
					t.Fatal(current)
				}
				lookups := x.base.lookups.Load()
				r = nativeJSONRequest(x.base.f.handler, "POST", endpoint, x.base.f.token, libraryTriggerBody(t, event, input))
				if r.Code != 200 {
					t.Fatal(r.Code, r.Body.String())
				}
				x.tick(t)
				if x.base.lookups.Load() != lookups {
					t.Fatal("legacy request replayed")
				}
				x.assertNoHistory(t)
			})
		}
	}
}
