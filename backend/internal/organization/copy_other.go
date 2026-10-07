//go:build !linux && !darwin

package organization

import "context"

func Copy(context.Context, Definition, Entry, string, func(Proof) error) (Proof, error) {
	return Proof{}, ErrState
}
func VerifyPublished(context.Context, Definition, Entry, Proof) error { return ErrState }
func Cleanup(Definition, Entry, Proof) error                          { return ErrState }
func TempName(string, int) string                                     { return "" }
