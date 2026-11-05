//go:build !darwin && !linux

package autobackup

import (
	"context"
	"os"
	"time"
)

type Stage struct{ Archive Archive }

func Review(string) (Target, error)        { return Target{}, ErrScope }
func Prepare(Target) (Target, error)       { return Target{}, ErrScope }
func (Target) Absent(string) (bool, error) { return false, ErrScope }
func Build(context.Context, Sources, Target, string, time.Time, bool) (*Stage, error) {
	return nil, ErrScope
}
func Open(context.Context, Target, Archive) (*os.File, error)                 { return nil, ErrScope }
func Remove(context.Context, Target, Archive, func(func() error) error) error { return ErrScope }
func (*Stage) Close()                                                         {}
func (*Stage) Publish() error                                                 { return ErrScope }
