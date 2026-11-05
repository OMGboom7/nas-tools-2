//go:build !darwin && !linux

package backuprestore

import "os"

func owned(os.FileInfo) bool                              { return false }
func openRegular(string) (*os.File, error)                { return nil, ErrState }
func (Workspace) lock(bool) (*os.File, error)             { return nil, ErrState }
func syncDir(string) error                                { return ErrState }
func moveExclusive(string, string) error                  { return ErrState }
func (Workspace) lockFile(string, bool) (*os.File, error) { return nil, ErrState }
