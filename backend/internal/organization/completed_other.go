//go:build !linux && !darwin

package organization

import "context"

func VerifyCompletedTarget(context.Context, Job, JobItem) error { return ErrState }
