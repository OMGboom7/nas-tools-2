//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package customhosts

import "os"

// Other platforms retain process-local serialization. Cross-process locking
// requires a platform implementation before claiming equivalent support.
func lockFile(*os.File) error { return nil }
func unlockFile(*os.File)     {}
