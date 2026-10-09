//go:build !linux && !darwin

package organization

import "context"

func Copy(context.Context, Definition, Entry, string, func(Proof) error) (Proof, error) {
	return Proof{}, ErrState
}
func Transfer(context.Context, Definition, Entry, string, func(Proof) error) (Proof, error) {
	return Proof{}, ErrMode
}
func TransferJournaled(context.Context, Definition, Entry, string, func(Proof) error, func(Proof) error) (Proof, error) {
	return Proof{}, ErrMode
}
func PrepareMoveTargetJournaled(context.Context, Definition, Entry, string, func(Proof) error, func(Proof) error) (Proof, error) {
	return Proof{}, ErrMode
}
func ValidateMode(context.Context, Definition, Entry) error           { return ErrMode }
func VerifyPublished(context.Context, Definition, Entry, Proof) error { return ErrState }
func Cleanup(Definition, Entry, Proof) error                          { return ErrState }
func TempName(string, int) string                                     { return "" }
func MoveHoldName(string, int) string                                 { return "" }
func PrepareMoveTarget(context.Context, Definition, Entry, string, func(Proof) error) (Proof, error) {
	return Proof{}, ErrMode
}
func PrepareMove(context.Context, Definition, Entry, Proof, string) (Proof, error) {
	return Proof{}, ErrMode
}
func ContinueMove(context.Context, Definition, Entry, Proof, bool, func() error) error {
	return ErrMode
}
func VerifyQuarantined(context.Context, Definition, Entry, Proof) error             { return ErrMode }
func CleanupMovedSource(context.Context, Definition, Entry, Proof) error            { return ErrMode }
func CleanupMoveTarget(context.Context, Definition, Entry, Proof) error             { return ErrMode }
func PublishPrepared(context.Context, Definition, Entry, Proof, func() error) error { return ErrMode }
func inspectUnpublished(context.Context, Definition, Entry, Proof, bool, func() error) error {
	return ErrMode
}
func inspectDiscarded(context.Context, Definition, Entry, Proof) error { return ErrMode }
