package httpserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

var errStatsScheduleChanged = errors.New("statistics schedule changed")

func holdStandaloneStatsClock(ctx context.Context, dbPath string) (func(), error) {
	return holdVolumeReceiver(ctx, dbPath, statsTimerGuard, ".nastool-stats-timer.lock")
}

const statsTimerGuard = "site-statistics-timer-owner"

func statsTimerCron(cfg map[string]any) string {
	value := objectValue(cfg["pt"])["ptrefresh_date_cron"]
	if value == false {
		return ""
	}
	raw := strings.TrimSpace(text(value))
	if raw == "0" {
		return ""
	}
	return raw
}

type statsTimer struct {
	db       *sql.DB
	config   *config.Store
	location *time.Location
	dbPath   string
}

const statsTimerTable = `CREATE TABLE IF NOT EXISTS GO_LEGACY_STATS_TIMER(ID INTEGER PRIMARY KEY CHECK(ID=1),CRON TEXT NOT NULL,ZONE TEXT NOT NULL,NEXT TEXT NOT NULL)`

// Observe the persisted deadline. Changing a schedule starts its original
// one-minute delay; reopening an unchanged schedule preserves its phase.
func (s statsTimer) observe(ctx context.Context, cron string, now time.Time) (time.Time, error) {
	var prior, zone, raw string
	err := s.db.QueryRowContext(ctx, "SELECT CRON,ZONE,NEXT FROM GO_LEGACY_STATS_TIMER WHERE ID=1").Scan(&prior, &zone, &raw)
	absent := errors.Is(err, sql.ErrNoRows)
	if err != nil && !absent {
		return time.Time{}, err
	}
	if !absent && prior == cron && zone == s.location.String() {
		return time.Parse(time.RFC3339Nano, raw)
	}
	if cron == "" {
		if !absent {
			_, err = s.db.ExecContext(ctx, "DELETE FROM GO_LEGACY_STATS_TIMER WHERE ID=1 AND CRON=? AND ZONE=? AND NEXT=?", prior, zone, raw)
			return time.Time{}, err
		}
		return time.Time{}, nil
	}
	due := now.Add(time.Minute).UTC()
	var res sql.Result
	if absent {
		res, err = s.db.ExecContext(ctx, "INSERT INTO GO_LEGACY_STATS_TIMER VALUES(1,?,?,?) ON CONFLICT(ID) DO NOTHING", cron, s.location.String(), due.Format(time.RFC3339Nano))
	} else {
		res, err = s.db.ExecContext(ctx, "UPDATE GO_LEGACY_STATS_TIMER SET CRON=?,ZONE=?,NEXT=? WHERE ID=1 AND CRON=? AND ZONE=? AND NEXT=?", cron, s.location.String(), due.Format(time.RFC3339Nano), prior, zone, raw)
	}
	if err != nil {
		return time.Time{}, err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return time.Time{}, errStatsScheduleChanged
	}
	return due, err
}

// Commit the next occurrence before the first site request. Other processes
// can observe the same deadline but only one can consume it, including after a crash.
func (s statsTimer) claim(ctx context.Context, cron string, due, next time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE GO_LEGACY_STATS_TIMER SET NEXT=? WHERE ID=1 AND CRON=? AND ZONE=? AND NEXT=?", next.UTC().Format(time.RFC3339Nano), cron, s.location.String(), due.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errStatsScheduleChanged
	}
	return err
}

func (s statsTimer) runDue(ctx context.Context, path, cron string, schedule signinSchedule, due time.Time, readLine func() ([]byte, error), out *json.Encoder) error {
	release, err := holdStandaloneStatsClock(ctx, s.dbPath)
	if err != nil {
		return err
	}
	defer release()
	var current bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM GO_LEGACY_STATS_TIMER WHERE ID=1 AND CRON=? AND ZONE=? AND NEXT=?)", cron, s.location.String(), due.UTC().Format(time.RFC3339Nano)).Scan(&current); err != nil {
		return err
	}
	if !current {
		return errStatsScheduleChanged
	}
	if err := out.Encode(map[string]string{"kind": "targets"}); err != nil {
		return err
	}
	var consumedNext time.Time
	occurrence := statisticsOccurrence{cron: cron, claim: func(ctx context.Context) error {
		next, err := schedule.following(due, time.Now())
		if err != nil {
			return err
		}
		if err = s.claim(ctx, cron, due, next); err == nil {
			consumedNext = next
		}
		return err
	}}
	runErr := runStatistics(ctx, path, "summary", readLine, out, &occurrence)
	if runErr != nil && ctx.Err() == nil {
		if err := out.Encode(map[string]string{"kind": "failed"}); err != nil {
			return err
		}
	}
	// A slow round consumes intermediate ticks, like a single-instance job.
	now := time.Now()
	if !consumedNext.IsZero() && !consumedNext.After(now) && ctx.Err() == nil {
		next, err := schedule.following(consumedNext, now)
		if err == nil {
			err = s.claim(ctx, cron, consumedNext, next)
		}
		if runErr == nil {
			return err
		}
	}
	return runErr
}

// StatisticsSchedulerCommand owns the timer in standalone and paired legacy
// deployments. The original process refreshes its icon cache after each run. EOF cancels waiting, collection and delivery.
func StatisticsSchedulerCommand(parent context.Context, path string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	replies := make(chan []byte)
	go func() {
		defer cancel()
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case replies <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	readLine := func() ([]byte, error) {
		select {
		case line := <-replies:
			return line, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, statsTimerTable); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	location, err := rssWorkerLocation()
	if err != nil {
		return err
	}
	s := statsTimer{db: db, config: config.NewStore(path), location: location, dbPath: dbPath}
	encoder := json.NewEncoder(out)
	var warned bool
	for ctx.Err() == nil {
		delay := 5 * time.Second
		cfg, err := s.config.Snapshot()
		if err == nil {
			cron := statsTimerCron(cfg)
			schedule, parseErr := parseSigninSchedule(cron, location)
			if parseErr != nil {
				cron = ""
			}
			now := time.Now()
			due, e := s.observe(ctx, cron, now)
			err = e
			if err == nil && !due.IsZero() {
				if due.After(now) {
					delay = min(delay, time.Until(due))
				} else {
					err = s.runDue(ctx, path, cron, schedule, due, readLine, encoder)
					// Re-read settings and the committed deadline after a run.
					delay = 0
					if err != nil {
						delay = time.Second
					}
				}
			}
			if err == nil {
				err = parseErr
			}
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, errStatsScheduleChanged) {
			if !warned {
				slog.Warn("site statistics schedule unavailable; will recheck configuration")
			}
			warned = true
		} else {
			warned = false
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}
