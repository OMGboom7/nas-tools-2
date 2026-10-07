//go:build !linux && !darwin

package organization

import "io/fs"

func Identity(info fs.FileInfo) string { return "" }

const CopySupported = false
