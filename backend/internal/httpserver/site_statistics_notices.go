package httpserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

// StatisticsRunCommand uses a local pipe for operator-configured Jinja templates. Go owns recipients, transport and receipts.
func StatisticsRunCommand(ctx context.Context, path, mode string, in io.Reader, out io.Writer) error {
	if mode != "refresh" && mode != "summary" {
		return sitestats.ErrState
	}
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 4096), 4<<20)
	readLine := func() ([]byte, error) {
		if !lines.Scan() {
			return nil, io.ErrUnexpectedEOF
		}
		return lines.Bytes(), nil
	}
	return runStatistics(ctx, path, mode, readLine, json.NewEncoder(out), nil)
}

type statisticsOccurrence struct {
	cron  string
	claim func(context.Context) error
}

func runStatistics(ctx context.Context, path, mode string, readLine func() ([]byte, error), encoder *json.Encoder, occurrence *statisticsOccurrence) error {
	raw, err := readLine()
	if err != nil {
		return err
	}
	var targets []scheduledStatsTarget
	var selection struct {
		Sites []string `json:"sites"`
	}
	if json.Unmarshal(raw, &targets) != nil || targets == nil {
		if json.Unmarshal(raw, &selection) != nil || selection.Sites == nil {
			return sitestats.ErrState
		}
		targets = nil
	}
	id := sitelogin.Random()
	if id == "" {
		return sitestats.ErrState
	}
	reference := "legacy-stats:" + id
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	rows, err := collectScheduledStatistics(ctx, path, targets, selection.Sites, reference, mode == "summary", occurrence)
	if err != nil {
		return err
	}
	failed := 0
	for _, row := range rows {
		if row.Data["err_msg"] != nil {
			failed++
		}
	}

	dbPath := (config.Application{Path: path}).UserDatabasePath()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	render := func(template, title, message string) ([]byte, error) {
		if err := encoder.Encode(map[string]string{"kind": "render", "template": template, "title": title, "text": message}); err != nil {
			return nil, io.ErrClosedPipe
		}
		var reply struct {
			Body *string `json:"body"`
		}
		raw, err := readLine()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if json.Unmarshal(raw, &reply) != nil || reply.Body == nil || *reply.Body == "" {
			return nil, sitestats.ErrState
		}
		return []byte(*reply.Body), nil
	}
	if organization.CopySupported {
		locks, err := organization.OpenJobLocks(dbPath)
		if err != nil {
			return err
		}
		guard, err := locks.AcquireJob(ctx, businessNotificationGuard)
		if err != nil {
			return err
		}
		defer guard.Close()
	}
	dispatcher := businessNotifications{queue: bus.New(db), service: notificationService{client: &http.Client{Transport: http.DefaultTransport}, config: config.NewStore(path)}}
	for {
		pending, err := dispatcher.queue.PendingLegacyStatistics(ctx)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		if err = dispatcher.dispatch(ctx, pending, render); err != nil {
			return err
		}
	}
	return encoder.Encode(map[string]any{"kind": "done", "failed": failed})
}
