//go:build !linux && !darwin

package organization

import "context"

func inspectSourceRestore(context.Context, Definition, Entry, Proof, bool, func() error) (string, string, error) {
	return "", "", ErrMode
}
