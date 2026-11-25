//go:build !linux && !darwin

package mediaarchive

import (
	"context"
	"errors"
	"io"
	"os"
)

const Supported = false

func openDirectory(string, []string, bool) (*os.File, error) { return nil, errors.ErrUnsupported }
func List(context.Context, *os.File, string) ([]File, string, error) {
	return nil, "", errors.ErrUnsupported
}
func Remove(*os.File, string, string, string) error { return errors.ErrUnsupported }

func Generate(context.Context, string, string, bool, string, int, func(io.Writer) (int, error), func() error) (Result, error) {
	return Result{}, errors.ErrUnsupported
}
