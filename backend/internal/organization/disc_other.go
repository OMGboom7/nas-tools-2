//go:build !linux && !darwin

package organization

import "context"

func readDiscManifest(context.Context, string, string) (DiscManifest, error) {
	return DiscManifest{}, ErrMode
}
