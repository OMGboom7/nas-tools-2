//go:build !windows

package httpserver

import (
	"context"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func holdStandaloneStatsClock(ctx context.Context, dbPath string) (func(), error) {
	locks, err := organization.OpenJobLocks(dbPath)
	if err != nil {
		return nil, err
	}
	guard, err := locks.AcquireJob(ctx, statsTimerGuard)
	if err != nil {
		return nil, err
	}
	return guard.Close, nil
}
