//go:build !linux && !darwin

package remotestore

// Unsupported intake platforms do not cache executable digests without a
// platform-specific change stamp.
func binaryChangeStamp(string) (string, error) { return "", nil }
