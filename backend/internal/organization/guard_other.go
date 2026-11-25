//go:build !linux && !darwin

package organization

import "context"

type JobGuard struct{}

func (*JobGuard) Close()                                                {}
func (*JobLocks) AcquireJob(context.Context, string) (*JobGuard, error) { return nil, ErrMode }
