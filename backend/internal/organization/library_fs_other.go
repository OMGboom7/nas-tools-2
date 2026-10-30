//go:build !linux && !darwin

package organization

import (
	"context"
	"os"
)

func ScanLibrary(context.Context, LibraryDefinition) (LibraryScan, error) {
	return LibraryScan{}, ErrMode
}
func CaptureLibraryMedia(context.Context, LibraryDefinition, string) (LibraryMedia, error) {
	return LibraryMedia{}, ErrMode
}
func VerifyLibraryMedia(context.Context, LibraryDefinition, LibraryMedia) error { return ErrMode }
func ReadLibraryMedia(context.Context, LibraryDefinition, LibraryMedia, func(*os.File) ([]byte, error)) ([]byte, error) {
	return nil, ErrMode
}
func LibraryReadNFO(context.Context, LibraryDefinition, LibraryMedia, string) ([]byte, error) {
	return nil, ErrMode
}
func LibraryTarget(LibraryDefinition, LibraryMedia, string, string) (string, error) {
	return "", ErrMode
}
func LibraryExisting(context.Context, LibraryDefinition, LibraryMedia, string, string) (bool, error) {
	return false, ErrMode
}
func PublishLibrarySidecar(context.Context, LibraryDefinition, LibraryMedia, string, string, []byte, func(LibraryOutputProof) error, func() error) (LibraryOutputProof, string, error) {
	return LibraryOutputProof{}, "blocked", ErrMode
}
func RestoreLibrarySidecar(context.Context, LibraryDefinition, LibraryOutputProof, func() error) (string, error) {
	return "uncertain", ErrMode
}
func ObserveLibrarySidecar(context.Context, LibraryDefinition, LibraryOutputProof) (string, error) {
	return "uncertain", ErrMode
}
