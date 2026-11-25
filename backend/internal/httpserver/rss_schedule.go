package httpserver

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/crontab"
)

type rssSchedule struct {
	interval time.Duration
	cron     crontab.Schedule
}

func parseRSSSchedule(raw string, location *time.Location) (rssSchedule, error) {
	var s rssSchedule
	raw = strings.TrimSpace(raw)
	if crontab.IsDecimal(raw) {
		minutes, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || minutes == 0 || minutes > uint64((1<<63-1)/int64(time.Minute)) || location == nil || len(raw) > 512 {
			return s, errors.New("invalid or unsupported RSS schedule")
		}
		s.interval = time.Duration(minutes) * time.Minute
		return s, nil
	}
	var err error
	s.cron, err = crontab.Parse(raw, location)
	return s, err
}
func (s rssSchedule) next(after time.Time) time.Time {
	if s.interval > 0 {
		return after.Add(s.interval)
	}
	return s.cron.Next(after)
}
