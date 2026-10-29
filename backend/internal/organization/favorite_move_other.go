//go:build !linux && !darwin

package organization

import "context"

func favoriteSnapshot(context.Context, FavoriteMoveDefinition, string) ([]RecoveryNode, error) {
	return nil, ErrMode
}
func favoritePresence(FavoriteMoveDefinition, string) (bool, error) { return false, ErrMode }
func favoriteReviewFiles(context.Context, FavoriteMoveDefinition, FavoriteMoveProof, string) error {
	return ErrMode
}
func favoriteExecute(context.Context, FavoriteMoveDefinition, *FavoriteMoveProof, *string, func() error, func(string) error) error {
	return ErrMode
}
func FavoriteDirectoryExists(LibraryRoot, string) (bool, error)    { return false, ErrMode }
func FavoriteMovieDirectory(RecoveryScope, string) (string, error) { return "", ErrMode }
