package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/crontab"
)

// The old pt.ptsignin_cron initializer used the first time in a range and
// interpreted whole numbers as clock-hour steps rather than elapsed intervals.
func legacySigninCron(value any) (string, error) {
	var raw string
	switch v := value.(type) {
	case string:
		raw = strings.TrimSpace(v)
	case int, uint64, float64, bool:
		raw = fmt.Sprint(v)
	default:
		return "", errors.New("invalid legacy pt.ptsignin_cron")
	}
	var cron string
	switch {
	case crontab.IsDecimal(raw):
		cron = "0 */" + raw + " * * *"
	case len(strings.Fields(raw)) == 5:
		cron = raw
	case strings.ContainsAny(raw, ":-"):
		start, _, _ := strings.Cut(raw, "-")
		parts := strings.Split(strings.TrimSpace(start), ":")
		if len(parts) != 2 {
			return "", errors.New("invalid legacy sign-in start time")
		}
		hour, hErr := strconv.Atoi(strings.TrimSpace(parts[0]))
		minute, mErr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if hErr != nil || mErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			return "", errors.New("invalid legacy sign-in start time")
		}
		cron = fmt.Sprintf("%d %d * * *", minute, hour)
	default:
		cron = "30 8 * * *"
	}
	if _, err := crontab.Parse(cron, time.UTC); err != nil {
		return "", errors.New("unsupported legacy sign-in schedule")
	}
	return cron, nil
}
