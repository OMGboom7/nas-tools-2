//go:build !windows

package httpserver

import (
	"context"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func holdVolumeReceiver(ctx context.Context, dbPath, key, windowsFile string) (func(), error) {
	locks, err := organization.OpenJobLocks(dbPath)
	if err != nil {
		return nil, err
	}
	guard, err := locks.AcquireJob(ctx, key)
	if err != nil {
		return nil, err
	}
	return guard.Close, nil
}
