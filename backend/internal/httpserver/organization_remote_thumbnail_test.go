package httpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

type remoteThumbnailFixture struct {
	f                       *subscriptionRunFixture
	cfg                     config.Config
	job                     organization.Job
	source, target, account string
	s3                      *s3test.Server
	lookups, images         int
	still                   string
}

func newRemoteThumbnailFixture(t *testing.T, mode string, prepare ...func(string)) *remoteThumbnailFixture {
	t.Helper()
	return remoteThumbnailFileFixture(t, mode, "Show.S01E02.mkv", prepare...)
}

func remoteThumbnailFileFixture(t *testing.T, mode, file string, prepare ...func(string)) *remoteThumbnailFixture {
	t.Helper()
	x := &remoteThumbnailFixture{}
	var input organizationPlanInput
	logical := "/bucket/library"
	if organization.RemoteDriver(mode) == "minio" {
		x.f, x.s3, x.source, input, x.cfg = minioManual(t, mode, false)
		x.account = x.cfg.MinioConfigFile
	} else {
		x.f, x.source, x.target, input, x.account = remoteHTTPFixture(t, mode, false)
		logical = "/rclone-test/movies"
		x.cfg = config.Config{ApplicationConfigPath: x.f.path, DisableLegacy: true, RcloneConfigFile: x.account, RcloneBinary: os.Getenv("NASTOOL_TEST_RCLONE_BINARY")}
	}
	if err := config.NewStore(x.f.path).Update(map[string]any{"media.tv_path": logical, "media.nfo_poster": true}); err != nil {
		t.Fatal(err)
	}
	input.TargetID = subscriptionResourceKey("target:" + logical + ":TV")
	input.Path = file
	organizationFile(t, x.source, input.Path)
	for _, fn := range prepare {
		fn(filepath.Join(x.source, input.Path))
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(x.source, input.Path), old, old); err != nil {
		t.Fatal(err)
	}
	base := x.f.transport
	x.f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			x.lookups++
			return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","external_ids":{"tvdb_id":300}}`), nil
		}
		if r.URL.Path == "/3/tv/200/season/1" {
			return jsonResponse(r, `{"id":201,"season_number":1,"episodes":[{"id":203,"show_id":200,"season_number":1,"episode_number":2,"name":"Second","still_path":"`+x.still+`"}]}`), nil
		}
		if r.URL.Host == "image.tmdb.org" {
			x.images++
			return nil, errors.New("image download not expected")
		}
		return base.RoundTrip(r)
	})
	x.f.runner.scraper.transport = x.f.transport
	if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"tv":{"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
		t.Fatal(err)
	}
	status, p, body := organizationPreview(t, x.f, input)
	if status != 200 || !p.ScraperEnabled || len(p.Items) != 1 || p.Items[0].Status != "available" {
		t.Fatal(status, body)
	}
	x.job = organizationCreateJob(t, x.f, input, p)
	return x
}
func (x *remoteThumbnailFixture) execute(t *testing.T) {
	t.Helper()
	status, _, body := remoteHTTPAction(t, x.f, x.job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	var err error
	x.job, err = x.f.runner.organizer.jobs.Get(t.Context(), x.job.ID)
	if err != nil || x.job.State != "completed" {
		t.Fatal(x.job, err)
	}
}
func (x *remoteThumbnailFixture) read(t *testing.T, relative string) ([]byte, bool) {
	t.Helper()
	if x.s3 != nil {
		return x.s3.Get(strings.TrimPrefix(filepath.Join(x.job.TargetRoot, relative), "/"))
	}
	data, err := os.ReadFile(filepath.Join(x.target, relative))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return data, err == nil
}
func (x *remoteThumbnailFixture) write(t *testing.T, relative string, data []byte) {
	t.Helper()
	if x.s3 != nil {
		x.s3.Put(strings.TrimPrefix(filepath.Join(x.job.TargetRoot, relative), "/"), data)
		return
	}
	name := filepath.Join(x.target, relative)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (x *remoteThumbnailFixture) thumb() string {
	return strings.TrimSuffix(x.job.Items[0].Target, filepath.Ext(x.job.Items[0].Target)) + "-thumb.jpg"
}
func (x *remoteThumbnailFixture) receipts(t *testing.T) []organization.ScrapeReceipt {
	t.Helper()
	rows, err := x.f.runner.organizer.jobs.Scrapes(t.Context(), false)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	return rows
}
func (x *remoteThumbnailFixture) restart(t *testing.T) {
	t.Helper()
	var err error
	x.f.handler, err = buildHandler(x.cfg, x.f.transport, &x.f.runner)
	if err != nil {
		t.Fatal(err)
	}
	// Match the original isolated CLI environment. Worker references the same
	// organizer; no workstation aliases or credential overrides are inherited.
	if organization.RemoteDriver(x.job.Mode) == "rclone" {
		x.f.runner.organizer.rclone.Environment = []string{}
	}
}

func TestRemoteThumbnailAllProvidersAndModesUseVerifiedCapabilityAndDoNotReplay(t *testing.T) {
	for _, mode := range []string{"rclonecopy", "rclone", "miniocopy", "minio"} {
		t.Run(mode, func(t *testing.T) {
			x := newRemoteThumbnailFixture(t, mode)
			x.execute(t)
			calls := 0
			var retained *remotestore.VerifiedObject
			x.f.runner.scraper.remoteThumbnail = func(ctx context.Context, o *remotestore.VerifiedObject) ([]byte, error) {
				calls++
				retained = o
				data := make([]byte, o.Size())
				n, err := o.ReadAt(ctx, data, 0)
				if err != nil || n != len(data) || string(data) != "original-media" {
					t.Fatal(n, err)
				}
				return thumbnailJPEG(t), nil
			}
			x.f.runner.scraper.thumbnail = func(context.Context, *os.File) ([]byte, error) {
				t.Fatal("remote path entered local decoder")
				return nil, nil
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := x.receipts(t)
			if rows[0].State != "completed" || len(rows[0].Results) != 2 || calls != 1 || x.images != 0 {
				t.Fatal(rows, calls, x.images)
			}
			if b, ok := x.read(t, x.thumb()); !ok || scraper.ValidateImage(b, ".jpg") != nil {
				t.Fatal("thumbnail not published")
			}
			if _, err := retained.ReadAt(t.Context(), make([]byte, 1), 0); err == nil {
				t.Fatal("capability survived completed callback")
			}
			x.restart(t)
			x.f.runner.scraper.remoteThumbnail = func(context.Context, *remotestore.VerifiedObject) ([]byte, error) {
				calls++
				return thumbnailJPEG(t), nil
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || calls != 1 || x.lookups != 1 {
				t.Fatal("restart replayed frame or metadata", err, calls, x.lookups)
			}
			page := nativeJSONRequest(x.f.handler, "GET", "/api/v1/organization/scraper", x.f.token, "")
			if page.Code != 200 || strings.Contains(page.Body.String(), x.account) || strings.Contains(page.Body.String(), s3test.SecretKey) || strings.Contains(page.Body.String(), "private-capability") {
				t.Fatal("receipt disclosed private reader", page.Code)
			}
			var histories int
			if err := x.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&histories); err != nil || histories != 1 {
				t.Fatal("intake repeated", histories, err)
			}
		})
	}
}

func TestRemoteThumbnailPreservationFailuresAndChangedEvidenceNeverPublishOrRetry(t *testing.T) {
	for _, provider := range []string{"miniocopy", "rclonecopy"} {
		for _, mode := range []string{"existing", "decoder", "settings", "account", "target", "lost-receipt", "malformed-still"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				x := newRemoteThumbnailFixture(t, provider)
				x.execute(t)
				calls := 0
				if mode == "existing" {
					x.write(t, x.thumb(), []byte("unvalidated existing image"))
				}
				if mode == "malformed-still" {
					x.still = "../../not-an-image"
				}
				x.f.runner.scraper.remoteThumbnail = func(ctx context.Context, o *remotestore.VerifiedObject) ([]byte, error) {
					calls++
					data := make([]byte, 3)
					if _, err := o.ReadAt(ctx, data, 0); err != nil {
						return nil, err
					}
					switch mode {
					case "decoder":
						return nil, errors.New("private decoder/account diagnostic")
					case "settings":
						if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_pic":{"tv":{"episode_thumb":false}}}`); err != nil {
							t.Fatal(err)
						}
					case "account":
						raw, _ := os.ReadFile(x.account)
						if err := os.WriteFile(x.account, append(raw, '\n'), 0600); err != nil {
							t.Fatal(err)
						}
					case "target":
						x.write(t, x.job.Items[0].Target, []byte("different-data"))
					case "lost-receipt":
						if _, err := x.f.db.Exec(`CREATE TRIGGER lose_remote_thumb BEFORE UPDATE OF OUTPUTS ON GO_SCRAPE_JOBS WHEN EXISTS(SELECT 1 FROM json_each(NEW.OUTPUTS) o WHERE json_extract(o.value,'$.Name') LIKE '%-thumb.jpg' AND json_extract(o.value,'$.State')='created') BEGIN SELECT RAISE(IGNORE); END`); err != nil {
							t.Fatal(err)
						}
					}
					return thumbnailJPEG(t), nil
				}
				if err := x.f.runner.scraper.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				rows := x.receipts(t)
				data, present := x.read(t, x.thumb())
				if mode == "existing" {
					if calls != 0 || string(data) != "unvalidated existing image" || rows[0].State != "completed" {
						t.Fatal(rows, calls)
					}
				} else {
					if rows[0].State != "partial" || strings.Contains(rows[0].Reason, "private") {
						t.Fatal(rows)
					}
					if present != (mode == "lost-receipt") {
						t.Fatal("wrong publication after failure", mode, present)
					}
					if mode == "malformed-still" && calls != 0 {
						t.Fatal("malformed metadata authorized video read")
					}
				}
				before := calls
				x.restart(t)
				x.f.runner.scraper.remoteThumbnail = func(context.Context, *remotestore.VerifiedObject) ([]byte, error) {
					calls++
					return thumbnailJPEG(t), nil
				}
				if err := x.f.runner.scraper.tick(t.Context()); err != nil || calls != before {
					t.Fatal("failed/uncertain frame replayed", err, calls)
				}
			})
		}
	}
}

func TestRemoteThumbnailConfigurationRevocationAndStopCancelInFlightReadWithoutReplay(t *testing.T) {
	for _, mode := range []string{"stop", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			x := newRemoteThumbnailFixture(t, "miniocopy")
			x.execute(t)
			started := make(chan struct{})
			calls := 0
			x.f.runner.scraper.remoteThumbnail = func(ctx context.Context, _ *remotestore.VerifiedObject) ([]byte, error) {
				calls++
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- x.f.runner.scraper.tick(ctx) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("decoder not started")
			}
			if mode == "stop" {
				cancel()
			} else {
				if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_pic":{"tv":{"episode_thumb":false}}}`); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("read did not cancel after stop/revocation")
			}
			rows := x.receipts(t)
			if rows[0].State != "partial" {
				t.Fatal(rows)
			}
			if _, present := x.read(t, x.thumb()); present {
				t.Fatal("cancelled read published image")
			}
			x.restart(t)
			x.f.runner.scraper.remoteThumbnail = func(context.Context, *remotestore.VerifiedObject) ([]byte, error) { calls++; return nil, nil }
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || calls != 1 {
				t.Fatal("cancelled frame replayed", err, calls)
			}
		})
	}
}

func TestRemoteThumbnailRealFFmpegAcrossProvidersPublishesOnceAndGatesSubtitles(t *testing.T) {
	binaryPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, spec := range []struct{ mode, extension string }{{"miniocopy", ".mkv"}, {"rclone", ".mkv"}, {"miniocopy", ".mp4"}, {"rclone", ".mp4"}} {
		t.Run(spec.mode+spec.extension, func(t *testing.T) {
			x := remoteThumbnailFileFixture(t, spec.mode, "Show.S01E02"+spec.extension, func(name string) {
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				codec := "ffv1"
				if spec.extension == ".mp4" {
					codec = "mpeg4"
				}
				cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "183", "-c:v", codec, "-threads", "1", name)
				if data, err := cmd.CombinedOutput(); err != nil {
					t.Fatal("video fixture failed", err, string(data))
				}
				if spec.extension == ".mp4" {
					data, err := os.ReadFile(name)
					if err != nil {
						t.Fatal(err)
					}
					moov, mdat := bytes.Index(data, []byte("moov"))-4, bytes.Index(data, []byte("mdat"))-4
					if moov <= mdat || mdat < 0 {
						t.Fatal("not an end-moov fixture")
					}
					padding := make([]byte, 8<<20)
					binary.BigEndian.PutUint32(padding, uint32(len(padding)))
					copy(padding[4:], "free")
					data = append(append(append([]byte{}, data[:moov]...), padding...), data[moov:]...)
					if err := os.WriteFile(name, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			submissions := 0
			base := x.f.transport
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "csf.test" {
					submissions++
					data, exists := x.read(t, x.thumb())
					if !exists || scraper.ValidateImage(data, ".jpg") != nil {
						t.Fatal("subtitle overtook real thumbnail")
					}
					return jsonResponse(r, `{"job_id":"remote-thumbnail-subtitle"}`), nil
				}
				return base.RoundTrip(r)
			})
			x.f.transport = transport
			x.f.runner.scraper.transport = transport
			x.f.runner.subtitles.transport = transport
			for _, spec := range []struct{ method, path, body string }{{"POST", "/api/v1/plugins/ChineseSubFinder/install", ""}, {"PUT", "/api/v1/plugins/ChineseSubFinder", `{"values":{"host":"https://csf.test","api_key":"fixture-key"}}`}} {
				w := nativeJSONRequest(x.f.handler, spec.method, spec.path, x.f.token, spec.body)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			x.execute(t)
			if err := x.f.runner.subtitles.tick(t.Context()); err != nil || submissions != 0 {
				t.Fatal("subtitle did not wait", err, submissions)
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := x.receipts(t)
			if rows[0].State != "completed" {
				t.Fatal(rows)
			}
			data, exists := x.read(t, x.thumb())
			if !exists || scraper.ValidateImage(data, ".jpg") != nil {
				t.Fatal("real JPEG absent")
			}
			if err := x.f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 {
				t.Fatal("subtitle completion", err, submissions)
			}
			x.restart(t)
			x.f.runner.subtitles.transport = transport
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := x.f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 {
				t.Fatal("subtitle replay", err, submissions)
			}
			after, _ := x.read(t, x.thumb())
			if !bytes.Equal(data, after) {
				t.Fatal("restart changed image")
			}
		})
	}
}

func TestRemoteThumbnailMissingFFmpegAndShortVideoLeaveBoundedFailureReceipts(t *testing.T) {
	for _, mode := range []string{"missing", "short"} {
		t.Run(mode, func(t *testing.T) {
			var hooks []func(string)
			if mode == "short" {
				binary, err := exec.LookPath("ffmpeg")
				if err != nil {
					t.Skip("ffmpeg unavailable")
				}
				hooks = append(hooks, func(name string) {
					os.Remove(name)
					cmd := exec.CommandContext(t.Context(), binary, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "2", "-c:v", "ffv1", "-threads", "1", name)
					if b, err := cmd.CombinedOutput(); err != nil {
						t.Fatal(err, string(b))
					}
				})
			}
			x := newRemoteThumbnailFixture(t, "miniocopy", hooks...)
			x.execute(t)
			if mode == "missing" {
				t.Setenv("PATH", t.TempDir())
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := x.receipts(t)
			if rows[0].State != "partial" {
				t.Fatal(rows)
			}
			if _, present := x.read(t, x.thumb()); present {
				t.Fatal("failed decoder published image")
			}
			// The pending outbox is drained, so repeated ticks do not re-decode.
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
