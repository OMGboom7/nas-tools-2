//go:build !linux && !darwin

package organization

import "context"

func PublishSidecar(context.Context, Job, JobItem, string, string, []byte, func(SidecarProof) error, func() error) (SidecarProof, string, error) {
	return SidecarProof{}, "blocked", ErrMode
}
