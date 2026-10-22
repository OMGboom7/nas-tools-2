//go:build !linux && !darwin

package organization

import "context"

func recoveryLocalSnapshot(context.Context, RecoveryScope, string) ([]RecoveryNode, error) {
	return nil, ErrMode
}
func recoveryLocalInventory(context.Context, RecoveryScope, string) ([]RecoveryNode, error) {
	return nil, ErrMode
}
func recoveryLocalBrowse(context.Context, RecoveryScope, string) ([]RecoveryBrowseEntry, error) {
	return nil, ErrMode
}
func recoveryLocalPresence(RecoveryScope, string) (*RecoveryNode, error) { return nil, ErrMode }
func recoveryLocalCapture(context.Context, RecoveryDefinition, string, func() error) error {
	return ErrMode
}
func recoveryLocalRemove(context.Context, RecoveryScope, string, RecoveryNode, func() error) error {
	return ErrMode
}
func recoveryLocalReturn(context.Context, RecoveryDisposal, RecoveryNode, func() error) error {
	return ErrMode
}
