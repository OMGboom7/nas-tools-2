//go:build !linux && !darwin

package autosub

import (
	"context"
	"os"
)

func openModelFile(*os.Root, string) (*os.File, error)            { return nil, ErrEngine }
func publishModelObject(*os.Root, string, string, *os.File) error { return ErrEngine }

func openModelPart(*os.Root, string) (*os.File, error)                    { return nil, ErrEngine }
func lockModelObject(context.Context, *os.Root, string) (*os.File, error) { return nil, ErrEngine }
