//go:build !linux && !darwin

package mediaarchive

import (
	"context"
	"errors"
	"os"
)

const Supported = false

func openDirectory(string, []string) (*os.File, error) { return nil, errors.ErrUnsupported }
func List(context.Context, *os.File, string) ([]File, string, error) {
	return nil, "", errors.ErrUnsupported
}
func Remove(*os.File, string, string, string) error { return errors.ErrUnsupported }
