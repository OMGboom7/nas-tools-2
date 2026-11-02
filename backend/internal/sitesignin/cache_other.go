//go:build !unix

package sitesignin

import "os"

func cacheIdentity(info os.FileInfo) any { return info.Name() }
