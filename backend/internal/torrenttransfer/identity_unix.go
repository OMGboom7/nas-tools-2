//go:build linux || darwin

package torrenttransfer

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (string, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrState
	}
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}
