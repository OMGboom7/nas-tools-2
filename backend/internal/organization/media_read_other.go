//go:build !linux && !darwin

package organization

import (
	"context"
	"os"
)

func ExistingSidecar(context.Context, Job, JobItem, string, string) (bool, error) {
	return false, ErrMode
}
func ReadCompletedMedia(context.Context, Job, JobItem, func(*os.File) ([]byte, error)) ([]byte, error) {
	return nil, ErrMode
}
