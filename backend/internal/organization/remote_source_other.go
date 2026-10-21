//go:build !linux && !darwin

package organization

import (
	"context"
	"io"
)

func remoteSources(context.Context, Definition, Entry) ([]RemoteFile, []string, error) {
	return nil, nil, ErrMode
}
func readRemoteSource(context.Context, Definition, Entry, RemoteFile, func(io.Reader) error) error {
	return ErrMode
}
