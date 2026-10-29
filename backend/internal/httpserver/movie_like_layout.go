package httpserver

import (
	"context"
	"path"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/movielike"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

// The original plugin changes the favorite category name even while stopped.
// Preserve that layout for inventory and subsequent intake; saving/uninstalling
// does not move existing favorites back or make their media disappear.
func (service subscriptionService) favoriteDirectory(ctx context.Context) (string, error) {
	if service.systemConfig == nil {
		return "精选", nil
	}
	raw, err := service.systemConfig.Get(ctx, movielike.ConfigKey)
	if err != nil {
		return "", err
	}
	v, err := movielike.Values(raw)
	if err != nil {
		return "", err
	}
	value, ok := v["dir_name"]
	if !ok || value == nil || value == "" {
		return "精选", nil
	}
	name, ok := value.(string)
	if !ok || !movielike.Component(name) {
		return "", movielike.ErrState
	}
	return name, nil
}
func (api organizationAPI) favoriteTarget(ctx context.Context, root organizationRoot, target string, recognition *nativeRecognition, disc bool, remote *remotestore.Client) (string, error) {
	if recognition.kind == "tv" || recognition.libraryCategory == "" {
		return target, nil
	}
	favorite, err := api.service.favoriteDirectory(ctx)
	if err != nil {
		return "", err
	}
	prefix := recognition.libraryCategory + "/"
	normalized := strings.ReplaceAll(target, "\\", "/")
	if !strings.HasPrefix(normalized, prefix) {
		return target, nil
	}
	candidate := path.Join(favorite, strings.TrimPrefix(normalized, prefix))
	directory := path.Dir(candidate)
	if disc {
		directory = candidate
	}
	if remote != nil {
		node, err := remote.Entry(ctx, root.Path, directory)
		if err != nil {
			return "", err
		}
		if node == nil {
			return target, nil
		}
		if !node.IsDir {
			return "", organization.ErrPath
		}
		return candidate, nil
	}
	frozen, err := libraryRootSnapshot(root)
	if err != nil {
		return "", err
	}
	present, err := organization.FavoriteDirectoryExists(frozen, directory)
	if err != nil {
		return "", err
	}
	if present {
		return candidate, nil
	}
	return target, nil
}
