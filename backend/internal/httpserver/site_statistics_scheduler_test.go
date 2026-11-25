package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Each connection represents an independent data host reading the same volume.
func TestStatisticsTimerClaimReloadAndRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "user.db")
	open := func() statsTimer {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err = db.Exec(statsTimerTable); err != nil {
			t.Fatal(err)
		}
		return statsTimer{db: db, dbPath: path, location: time.UTC}
	}
	first, peer := open(), open()
	now := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	observe := func(s statsTimer, cron string, at time.Time) time.Time {
		due, err := s.observe(ctx, cron, at)
		if err != nil {
			t.Fatal(err)
		}
		return due
	}
	due := observe(first, "6", now)
	if due != now.Add(time.Minute) || observe(peer, "6", now.Add(15*time.Second)) != due {
		t.Fatal("first deadline changed", due)
	}
	complete := make(chan error, 2)
	for _, host := range []statsTimer{first, peer} {
		go func() { complete <- host.claim(ctx, "6", due, due.Add(6*time.Hour)) }()
	}
	won, lost := 0, 0
	for range 2 {
		err := <-complete
		switch {
		case err == nil:
			won++
		case errors.Is(err, errStatsScheduleChanged):
			lost++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatal(won, lost)
	}
	reopened := open()
	if next := observe(reopened, "6", now.Add(time.Hour)); next != due.Add(6*time.Hour) {
		t.Fatal("claim replayed", next)
	}
	changed := observe(first, "0 8 * * *", now.Add(2*time.Hour))
	if changed != now.Add(2*time.Hour+time.Minute) {
		t.Fatal("new rule retained old deadline", changed)
	}
	if err := peer.claim(ctx, "6", due.Add(6*time.Hour), due.Add(12*time.Hour)); !errors.Is(err, errStatsScheduleChanged) {
		t.Fatal("stale rule claimed", err)
	}
	if next := observe(reopened, "", now); !next.IsZero() {
		t.Fatal(next)
	}
	if err := first.claim(ctx, "0 8 * * *", changed, changed.Add(24*time.Hour)); !errors.Is(err, errStatsScheduleChanged) {
		t.Fatal("disabled rule claimed", err)
	}
}

func TestStatisticsTimerRespectsNativeClockOwnership(t *testing.T) {
	if !organization.CopySupported {
		t.Skip("pure Go statistics worker requires job guards")
	}
	f, api := newStatsFixture(t)
	if err := f.api.config.Update(map[string]any{"pt.ptrefresh_date_cron": "6"}); err != nil {
		t.Fatal(err)
	}
	state := statsState(t, f)
	r := f.request(t, "POST", "/api/v1/site-statistics/activate", map[string]any{"fingerprint": state.Fingerprint, "expectedEpoch": "", "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	clock := nativeStatsClock{api: api}
	defer clock.close()
	clock.tick(t.Context(), time.Now())
	if clock.guard == nil {
		t.Fatal("native timer did not acquire the volume")
	}
	if release, err := holdStandaloneStatsClock(context.Background(), f.path); err == nil {
		release()
		t.Fatal("both schedulers acquired the same volume")
	}
	clock.close()
	release, err := holdStandaloneStatsClock(t.Context(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestStatisticsTimerDisabledDuringRequestStopsCollection(t *testing.T) {
	began := make(chan struct{})
	done := make(chan error, 1)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(began)
		<-r.Context().Done()
	}))
	defer tracker.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("pt: {ptrefresh_date_cron: '6'}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Test", SignURL: tracker.URL, Include: "T", Cookie: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	go func() {
		_, err := collectScheduledStatistics(ctx, path, []scheduledStatsTarget{{site.ID, site.Name, tracker.URL}}, nil, "legacy-stats:fixture", true, &statisticsOccurrence{cron: "6", claim: func(context.Context) error { return nil }})
		done <- err
	}()
	select {
	case <-began:
	case err := <-done:
		t.Fatal("request never started", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := config.NewStore(path).Update(map[string]any{"pt.ptrefresh_date_cron": ""}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("disabled run completed", err)
		}
	case <-ctx.Done():
		t.Fatal("did not cancel outstanding site request")
	}
}
