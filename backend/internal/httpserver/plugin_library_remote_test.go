package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestRemoteLibraryPluginOnceAndCronRequireFreshIndependentProviderGrant(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := newRemoteLibraryHTTP(t, driver, false)
			x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
			libraryPluginInstall(t, x.base)
			values := libraryPluginValues(x.base, true, "0 0 1 1 *", "force_nfo")
			values["scraper_path"] = []string{driver + ":" + x.base.input.RootID}
			detail := libraryPluginDetail(t, x.base)
			raw, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: values})
			if r := nativeJSONRequest(x.base.f.handler, "PUT", libraryPluginEndpoint, x.base.f.token, string(raw)); r.Code != 400 {
				t.Fatal("native local consent expanded to remote", r.Code, r.Body.String())
			}
			values["go_remote_exclusive"], values["go_remote_recovery"] = true, true
			c := libraryPluginSave(t, x.base, values)
			if c.RemoteGrant == nil || !c.RemoteGrant.Valid() || len(c.Lease.Templates) != 1 || c.Lease.Templates[0].Remote == nil || c.Lease.Templates[0].Remote.Driver != driver {
				t.Fatal(c)
			}
			now := c.OnceAt
			if c.Next.Before(now) {
				now = c.Next
			}
			libraryPluginTick(t, x.base, now.Add(time.Second))
			current := libraryPluginControlRead(t, x.base)
			if current.State != "queued" || len(current.Jobs) != 1 {
				t.Fatal(current)
			}
			x.tick(t)
			job := x.base.job(t, current.Jobs[0])
			if job.State != "completed" || job.Remote == nil || job.Remote.Driver != driver {
				t.Fatal(job)
			}
			lookups := x.base.lookups.Load()
			x.restart(t)
			libraryPluginTick(t, x.base, now.Add(2*time.Second))
			x.tick(t)
			if x.base.lookups.Load() != lookups {
				t.Fatal("consumed once request replayed on restart")
			}
			current = libraryPluginControlRead(t, x.base)
			previous := current.Batch
			libraryPluginTick(t, x.base, current.Next.Add(time.Second))
			current = libraryPluginControlRead(t, x.base)
			if current.State != "queued" || current.Batch == previous || len(current.Jobs) != 1 {
				t.Fatal("next cron occurrence did not create a fresh scope", current)
			}
			x.tick(t)
			if job := x.base.job(t, current.Jobs[0]); job.State != "completed" {
				t.Fatal(job)
			}
			x.assertNoHistory(t)
		})
	}
}

func TestRemoteLibraryPluginDoesNotAdoptAnOldAuthorizedControlWithoutRemoteGrant(t *testing.T) {
	x := newRemoteLibraryHTTP(t, "minio", false)
	x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
	libraryPluginInstall(t, x.base)
	values := libraryPluginValues(x.base, true, "", "force_all")
	values["scraper_path"] = []string{"minio:" + x.base.input.RootID}
	values["go_remote_exclusive"], values["go_remote_recovery"] = true, true
	c := libraryPluginSave(t, x.base, values)
	now := c.OnceAt
	c.RemoteGrant = nil
	raw, _ := libraryControlJSON(c)
	if err := x.base.f.runner.scraper.system.Set(t.Context(), libraryControlKey, raw); err != nil {
		t.Fatal(err)
	}
	libraryPluginTick(t, x.base, now.Add(time.Second))
	c = libraryPluginControlRead(t, x.base)
	if c.Authorized || c.State != "paused" {
		t.Fatal("old schedule obtained remote permission", c)
	}
	jobs, err := x.base.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}

func TestRemoteLibraryMixedProviderBatchQueuesAllScopesAndNeverInventsTransfers(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit isolated Rclone required")
	}
	x := newRemoteLibraryHTTP(t, "minio", false)
	x.put(t, "Movie/Movie.2026.mkv", []byte("original-media"))
	alias := t.TempDir()
	libraryWriteRoot := filepath.Join(alias, "bucket/series")
	organizationFile(t, libraryWriteRoot, "Show/Season 01/Show.S01E02.mkv")
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[NASTOOL]\ntype=alias\nremote="+alias+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	x.cfg.RcloneConfigFile, x.cfg.RcloneBinary = conf, binary
	if err := config.NewStore(x.base.f.path).Update(map[string]any{"media.tv_path": "/bucket/series", "media.anime_path": "/bucket/series"}); err != nil {
		t.Fatal(err)
	}
	x.restart(t)
	libraryPluginInstall(t, x.base)
	values := libraryPluginValues(x.base, true, "", "no_force")
	values["scraper_path"] = []string{"minio:" + x.base.input.RootID, "rclone:" + subscriptionResourceKey("target:/bucket/series:TV")}
	values["go_remote_exclusive"], values["go_remote_recovery"] = true, true
	c := libraryPluginSave(t, x.base, values)
	if len(c.Lease.Templates) != 2 {
		t.Fatal(c)
	}
	libraryPluginTick(t, x.base, c.OnceAt.Add(time.Second))
	c = libraryPluginControlRead(t, x.base)
	if len(c.Jobs) != 2 || c.State != "queued" {
		t.Fatal("mixed scope partially registered", c)
	}
	x.tick(t)
	for _, id := range c.Jobs {
		job := x.base.job(t, id)
		if job.State != "completed" || job.Remote == nil {
			t.Fatal(job)
		}
	}
	data, err := os.ReadFile(filepath.Join(libraryWriteRoot, "Show/Season 01/Show.S01E02.mkv"))
	if err != nil || string(data) != "original-media" {
		t.Fatal("mixed remote source changed", err)
	}
	x.assertNoHistory(t)
}
