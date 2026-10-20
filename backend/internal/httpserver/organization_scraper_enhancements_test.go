package httpserver

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

func advancedScrapeFixture(t *testing.T, mode string, prepare ...func(string)) *intakeScrapeFixture {
	t.Helper()
	x := newIntakeScrapeFixture(t, mode, true, prepare...)
	base := x.f.transport
	x.f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/3/tv/200/season/1" {
			return jsonResponse(r, `{"id":201,"season_number":1,"episodes":[{"id":203,"show_id":200,"season_number":1,"episode_number":2,"name":"Second"}]}`), nil
		}
		return base.RoundTrip(r)
	})
	monitorRebuild(t, x.f)
	if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
		t.Fatal(err)
	}
	return x
}
func thumbnailJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 9)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestIntakeScraperThumbnailFallbackAllModesUsesPinnedReadAndPersistentPublication(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			x := advancedScrapeFixture(t, mode)
			calls := 0
			x.f.runner.scraper.thumbnail = func(_ context.Context, f *os.File) ([]byte, error) {
				calls++
				b, err := io.ReadAll(f)
				if err != nil || string(b) != "original-media" {
					t.Fatal("wrong media read", string(b), err)
				}
				if _, err := f.Write([]byte("no")); err == nil {
					t.Fatal("writable media handle")
				}
				return thumbnailJPEG(t), nil
			}
			x.execute(t)
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := x.receipts(t)
			if len(rows) != 1 || rows[0].State != "completed" || len(rows[0].Results) != 4 || calls != 1 || x.images != 0 {
				t.Fatal(rows, calls, x.images)
			}
			thumb := strings.TrimSuffix(x.nfo(t), ".nfo") + "-thumb.jpg"
			b, err := os.ReadFile(thumb)
			if err != nil || scraper.ValidateImage(b, ".jpg") != nil {
				t.Fatal(err)
			}
			monitorRebuild(t, x.f)
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || calls != 1 {
				t.Fatal("renderer repeated on restart", err, calls)
			}
		})
	}
}
func TestIntakeScraperThumbnailPreservationFailureAndChangedConfigurationNeverReplay(t *testing.T) {
	for _, mode := range []string{"existing", "failure", "settings", "lost-receipt"} {
		t.Run(mode, func(t *testing.T) {
			x := advancedScrapeFixture(t, "copy")
			calls := 0
			x.execute(t)
			thumb := strings.TrimSuffix(x.nfo(t), ".nfo") + "-thumb.jpg"
			if mode == "existing" {
				if err := os.WriteFile(thumb, []byte("existing image retained"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			x.f.runner.scraper.thumbnail = func(context.Context, *os.File) ([]byte, error) {
				calls++
				switch mode {
				case "failure":
					return nil, errors.New("private decoder diagnostic")
				case "settings":
					if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_pic":{"tv":{"episode_thumb":false}}}`); err != nil {
						t.Fatal(err)
					}
				case "lost-receipt":
					if _, err := x.f.db.Exec(`CREATE TRIGGER lose_thumb_result BEFORE UPDATE OF OUTPUTS ON GO_SCRAPE_JOBS WHEN EXISTS(SELECT 1 FROM json_each(NEW.OUTPUTS) o WHERE json_extract(o.value,'$.Name') LIKE '%-thumb.jpg' AND json_extract(o.value,'$.State')='created') BEGIN SELECT RAISE(IGNORE); END`); err != nil {
						t.Fatal(err)
					}
				}
				return thumbnailJPEG(t), nil
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			before := calls
			monitorRebuild(t, x.f)
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || calls != before {
				t.Fatal("failed frame replayed", err, calls)
			}
			rows := x.receipts(t)
			if mode == "existing" {
				b, _ := os.ReadFile(thumb)
				if calls != 0 || string(b) != "existing image retained" || rows[0].State != "completed" {
					t.Fatal(rows, calls)
				}
			} else {
				if rows[0].State != "partial" || strings.Contains(rows[0].Reason, "private") {
					t.Fatal(rows)
				}
				if mode == "lost-receipt" {
					if _, err := os.Stat(thumb); err != nil {
						t.Fatal("published image removed after receipt failure", err)
					}
				} else if _, err := os.Stat(thumb); !os.IsNotExist(err) {
					t.Fatal("failed frame published", err)
				}
			}
		})
	}
}
func TestIntakeScraperChineseCreditsActuallyPublishSimplifiedNFOWithOriginalIDs(t *testing.T) {
	x := newIntakeScrapeFixture(t, "copy", false)
	base := x.f.transport
	x.f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "frodo.douban.com" {
			return jsonResponse(r, `{"items":[]}`), nil
		}
		if strings.HasPrefix(r.URL.Path, "/3/person/") {
			id := strings.TrimPrefix(r.URL.Path, "/3/person/")
			return jsonResponse(r, `{"id":`+id+`,"also_known_as":["陳 曉 華"]}`), nil
		}
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","credits":{"cast":[{"id":2,"name":"John Smith","known_for_department":"Acting","order":0,"character":"Original role"}],"crew":[]}}`), nil
		}
		return base.RoundTrip(r)
	})
	monitorRebuild(t, x.f)
	if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":true,"credits":true,"credits_chinese":true}}}`); err != nil {
		t.Fatal(err)
	}
	x.execute(t)
	if err := x.f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := x.receipts(t)
	b, err := os.ReadFile(x.nfo(t))
	if err != nil || rows[0].State != "completed" || !strings.Contains(string(b), "<name>陈晓华</name>") || !strings.Contains(string(b), "<tmdbid>2</tmdbid>") || !strings.Contains(string(b), "<role>Original role</role>") {
		t.Fatal(string(b), rows, err)
	}
}
func TestIntakeScraperRealFFmpegMissingStillProducesOnePublishedJPEG(t *testing.T) {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable; descriptor/receipt tests still run")
	}
	x := advancedScrapeFixture(t, "copy", func(root string) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		path := filepath.Join(root, "Show.S01E02.mkv")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, binary, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "183", "-c:v", "ffv1", "-threads", "1", path)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatal("synthetic video failed", err, string(b))
		}
	})
	x.execute(t)
	if err := x.f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := x.receipts(t)
	if rows[0].State != "completed" {
		t.Fatal(rows)
	}
	path := strings.TrimSuffix(x.nfo(t), ".nfo") + "-thumb.jpg"
	b, err := os.ReadFile(path)
	if err != nil || scraper.ValidateImage(b, ".jpg") != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	monitorRebuild(t, x.f)
	if err := x.f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if info.ModTime() != after.ModTime() {
		t.Fatal("real video replayed after restart")
	}
}
